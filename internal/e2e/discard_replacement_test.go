//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/app"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestApprovedDiscardPreservesSameNamedReplacementBeforeSignal(t *testing.T) {
	f := newWorkerTabsFixture(t)
	ctx := context.Background()
	task := f.start(t, "t1", "Cancel replacement guard", "cancel-replacement-guard")
	ownedPID := foregroundPID(t, f, task.PaneID)
	work := filepath.Join(task.WorktreePath, "replacement-work.txt")
	if err := os.WriteFile(work, []byte("preserve replacement work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := &t175Adapter{Client: f.client, target: task.PaneID}
	replacementPID := 0
	adapter.beforeSnapshot = func() {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := db.IntentByTask(ctx, task.ID)
		_ = db.Close()
		if replacementPID != 0 || err != nil || intent.Step != "in_progress:worker.stop" {
			return
		}
		f.exitWorker(t, "t1")
		if err := os.Remove(filepath.Join(task.WorktreePath, ".posse-e2e-agent-exit")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Call(ctx, "pane.clear_agent_authority", map[string]any{"pane_id": task.PaneID}); err != nil {
			t.Fatal(err)
		}
		// Keep kind, label and launch name identical. Only the process instance
		// changes, so name-based or group-wide shutdown would destroy it.
		if _, err := f.client.Run(ctx, "agent", "start", "signal-replacement", "--kind", "claude", "--pane", task.PaneID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Run(ctx, "agent", "rename", task.PaneID, task.AgentName); err != nil {
			t.Fatal(err)
		}
		replacementPID = foregroundPID(t, f, task.PaneID)
	}
	for _, entry := range f.leadEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			t.Setenv(key, value)
		}
	}
	t.Chdir(f.repo)
	cli := app.New(f.home, adapter).CLI()
	var output bytes.Buffer
	cli.Out, cli.ErrOut = &output, &output
	code := cli.Run([]string{"teardown", "t1", "--discard", "--user-approved", "destroy t3 now"})
	if code == 0 || !strings.Contains(output.String(), "Replacement foreground process") || replacementPID <= 1 || replacementPID == ownedPID {
		t.Fatalf("same-named replacement was not refused: original=%d replacement=%d code=%d output=%s", ownedPID, replacementPID, code, output.String())
	}
	if _, _, err := store.ProcessIdentityForPID(replacementPID); err != nil {
		t.Fatalf("replacement process was stopped: %v", err)
	}
	if contents, err := os.ReadFile(work); err != nil || string(contents) != "preserve replacement work\n" {
		t.Fatalf("replacement work changed: %q %v", contents, err)
	}
	if current := f.task(t, "t1"); current.State != store.StateWorking {
		t.Fatalf("refusal changed Task state: %#v", current)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sandbox, err := db.SandboxByTask(ctx, task.ID)
	if err != nil || sandbox.State != "held" {
		t.Fatalf("replacement Sandbox was released: %#v %v", sandbox, err)
	}
	gitTest(t, f.env, f.repo, "rev-parse", "--verify", "refs/heads/"+task.Branch)
	f.assertAlive(t, f.snapshot(t), task.PaneID, f.leadPaneID)
}

func TestApprovedWorkingDiscardResumesAfterCrash(t *testing.T) {
	for _, step := range []string{"worker.stop", "panes.close"} {
		for _, phase := range []string{"before", "after"} {
			t.Run(step+"/"+phase, func(t *testing.T) {
				f := newWorkerTabsFixture(t)
				task := f.start(t, "t1", "Cancel crash recovery", "cancel-crash-recovery")
				// Exercise explicit retry without a watch concurrently repairing
				// the same deliberately interrupted intent.
				db, err := store.Open(f.home)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				f.env = setEnv(f.env, "POSSE_WATCH_TOKEN", "e2e-manual-owner")
				f.leadEnv = setEnv(f.leadEnv, "POSSE_WATCH_TOKEN", "e2e-manual-owner")
				stopFixtureBackgroundWatch(t, f.root, f.env, db, f.projectID)
				command := exec.Command(f.binary, "teardown", "t1", "--discard", "--user-approved", "destroy t3 now")
				command.Dir = f.repo
				command.Env = setEnv(f.leadEnv, "POSSE_INTENT_CRASH_AT", "teardown:"+phase+":"+step)
				output, err := command.CombinedOutput()
				if err == nil || !strings.Contains(err.Error(), "exit status 86") {
					t.Fatalf("cancellation missed crash boundary: %v %s", err, output)
				}
				outputText := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1", "--discard", "--user-approved", "destroy t3 now")
				if current := f.task(t, "t1"); current.State != store.StateTornDown {
					t.Fatalf("cancellation did not resume: %#v output=%s", current, outputText)
				}
				f.assertGone(t, f.snapshot(t), task.PaneID)
				f.assertAlive(t, f.snapshot(t), f.leadPaneID)
			})
		}
	}
}
