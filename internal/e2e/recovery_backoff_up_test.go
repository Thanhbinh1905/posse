//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestUpStartsLeadAndSettlesRelaunchIntentWhileRecoveryWaitsForBackoff(t *testing.T) {
	f := newRiderTabsFixture(t)
	t.Cleanup(func() { stopIsolatedFinalizers(t, f.root) })
	startLog := filepath.Join(f.root, "recovery-starts.log")
	agentPath := filepath.Join(f.root, "bin", "claude")
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	instrument := `case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*) printf 'worker\n' >> "$POSSE_TEST_ROOT/recovery-starts.log" ;;
  *) printf 'lead\n' >> "$POSSE_TEST_ROOT/recovery-starts.log" ;;
esac
`
	agent = []byte(strings.Replace(string(agent), "#!/bin/sh\n", "#!/bin/sh\n"+instrument, 1))
	if err := os.WriteFile(agentPath, agent, 0o700); err != nil {
		t.Fatal(err)
	}

	initial := f.ride(t, "t1", "Recovery waits for backoff", "recovery-waits-backoff")
	workerStarts := func() int {
		t.Helper()
		contents, err := os.ReadFile(startLog)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Count(string(contents), "worker\n")
	}
	startsBeforeCrash := workerStarts()
	if startsBeforeCrash != 1 {
		t.Fatalf("initial Rider starts=%d, want 1", startsBeforeCrash)
	}

	backoffEnds := time.Now().Add(time.Minute).UnixMilli()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claimed, err := db.ClaimTaskRecovery(context.Background(), initial.ID, f.snapshot(t).ServerStartedAt, 0, os.Getpid(), 3, now.UnixMilli(), backoffEnds)
	if err == nil && !claimed {
		err = fmt.Errorf("could not seed a pending recovery attempt")
	}
	if err == nil {
		err = db.FinishTaskRecovery(context.Background(), initial, os.Getpid(), 3, false, "injected transient recovery failure", backoffEnds)
	}
	closeErr := db.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	crashEnv := setEnv(f.leadEnv, "POSSE_INTENT_CRASH_AT", "relaunch:after:agent.start")
	command := exec.Command(f.binary, "relaunch", "t1")
	command.Dir, command.Env = f.repo, crashEnv
	output, commandErr := command.CombinedOutput()
	exit, exited := commandErr.(*exec.ExitError)
	if !exited || exit.ExitCode() != 86 {
		t.Fatalf("explicit relaunch did not crash after agent.start: err=%v output=%s", commandErr, output)
	}
	if got := workerStarts(); got != startsBeforeCrash+1 {
		t.Fatalf("crashed relaunch starts=%d, want one new Rider launch", got-startsBeforeCrash)
	}
	if err := func() error {
		db, err := store.Open(f.home)
		if err != nil {
			return err
		}
		defer db.Close()
		intent, err := db.IntentByTask(context.Background(), initial.ID)
		if err != nil {
			return err
		}
		if intent.Command != "relaunch" || intent.Step != "done:agent.start" {
			return fmt.Errorf("interrupted relaunch intent = %#v", intent)
		}
		recovery, err := db.TaskRecovery(context.Background(), initial.ID)
		if err != nil {
			return err
		}
		if recovery.Status != "pending" || recovery.NextAttemptAt <= time.Now().UnixMilli() {
			return fmt.Errorf("recovery is not waiting for backoff: %#v", recovery)
		}
		return nil
	}(); err != nil {
		t.Fatal(err)
	}

	closeRiderGroup(t, f)
	caller, err := createWorkspace(f.client, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Run(context.Background(), "pane", "run", caller.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
		t.Fatalf("run replacement posse up in isolated Herdr pane: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		snapshot, err := f.client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		_, found := findLeadInSnapshot(snapshot)
		return found
	}) {
		t.Fatal("replacement Lead did not start in the User's workspace")
	}
	if contents, err := os.ReadFile(startLog); err != nil || strings.Count(string(contents), "lead\n") != 1 {
		t.Fatalf("Lead starts=%q err=%v, want exactly one replacement Lead", contents, err)
	}
	if got := workerStarts(); got != startsBeforeCrash+2 {
		t.Fatalf("Rider starts after replacement up=%d, want one launch to settle the interrupted intent", got-startsBeforeCrash)
	}

	task := f.task(t, "t1")
	if task.State != store.StateWorking || task.Launches != initial.Launches+2 {
		t.Fatalf("interrupted Rider relaunch was not settled exactly once: initial=%#v current=%#v", initial, task)
	}
	snapshot := f.snapshot(t)
	lead, found := findLeadInSnapshot(snapshot)
	if !found {
		t.Fatal("replacement Lead is not live")
	}
	f.assertAlive(t, snapshot, task.PaneID, lead.PaneID)
	if err := func() error {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.IntentByTask(context.Background(), task.ID)
		if !store.IsNotFound(err) {
			return fmt.Errorf("relaunch intent remains after startup: %v", err)
		}
		return nil
	}(); err != nil {
		t.Fatal(err)
	}
}

// A detached Lead startup finalizer can outlive this isolated fixture. Stop
// only finalizers whose environment points into the fixture root.
func stopIsolatedFinalizers(t *testing.T, root string) {
	t.Helper()
	details, err := fixtureProcessDetails(root)
	if err != nil {
		t.Errorf("inspect isolated processes: %v", err)
		return
	}
	for _, detail := range details {
		if !strings.Contains(detail, "_finalize-lead") {
			continue
		}
		pidText, _, _ := strings.Cut(detail, ":")
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			t.Errorf("parse isolated Lead finalizer PID %q: %v", pidText, err)
			continue
		}
		process, err := os.FindProcess(pid)
		if err == nil {
			_ = process.Kill()
		}
	}
}
