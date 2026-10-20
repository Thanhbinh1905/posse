//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConcurrentStartsDoNotReportSandboxMissingDuringCheckout(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	blockedPath := filepath.Join(f.home, "pool", "shop", "sandbox-1")
	entered := filepath.Join(f.root, "worktree-add-entered")
	release := filepath.Join(f.root, "release-worktree-add")
	shim := fmt.Sprintf("#!/bin/sh\nif [ \"$3\" = worktree ] && [ \"$4\" = add ] && [ \"$9\" = %s ]; then\n  : > %s\n  while [ ! -e %s ]; do sleep 0.01; done\nfi\nexec %s \"$@\"\n", shellQuote(blockedPath), shellQuote(entered), shellQuote(release), shellQuote(realGit))
	if err := os.WriteFile(filepath.Join(f.root, "bin", "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}

	startCommand := func(name string) (*exec.Cmd, *bytes.Buffer) {
		t.Helper()
		brief := filepath.Join(f.root, name+".md")
		contents := "---\ntype: change\ntitle: " + name + "\ndone_when: the test finishes\n---\nWait.\n"
		if err := os.WriteFile(brief, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(f.binary, "start", "--brief", brief, "--name", name)
		command.Dir, command.Env = f.repo, f.leadEnv
		output := &bytes.Buffer{}
		command.Stdout, command.Stderr = output, output
		return command, output
	}

	first, firstOutput := startCommand("concurrent-1")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(release, nil, 0o600)
		if first.Process != nil {
			_ = first.Process.Kill()
			_ = first.Wait()
		}
	}()
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(entered)
		return err == nil
	}) {
		t.Fatalf("first posse start did not pause inside git worktree add: %s", firstOutput.String())
	}
	if _, err := os.Stat(filepath.Join(blockedPath, ".git")); !os.IsNotExist(err) {
		t.Fatalf("blocked checkout unexpectedly exists before git worktree add: %v", err)
	}

	// Run three more real CLI starts while the first has claimed its Sandbox
	// row but Git has not materialized the checkout metadata.
	for index := 2; index <= 4; index++ {
		command, output := startCommand(fmt.Sprintf("concurrent-%d", index))
		if err := command.Run(); err != nil {
			t.Fatalf("concurrent posse start %d: %v\n%s", index, err, output.String())
		}
	}

	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "sandbox_missing" {
			t.Fatalf("concurrent start reported its still-creating Sandbox as missing: %#v", notice)
		}
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first posse start: %v\n%s", err, firstOutput.String())
	}
	firstDone := false
	for _, task := range []string{"t1", "t2", "t3", "t4"} {
		f.waitState(t, task, store.StateWorking)
		if task == "t1" {
			firstDone = true
		}
	}
	if !firstDone {
		t.Fatal("first Task did not complete its start")
	}

	// Simulate a false-positive Notice from an older build. A successful
	// reconciliation of the now-present checkout must resolve it so a later
	// real disappearance is not hidden by HasNotice.
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: f.projectID, TaskID: f.task(t, "t1").ID, Kind: "sandbox_missing", Summary: "stale false-positive from an earlier reconcile", DataJSON: `{}`}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "list")
	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	staleOpen, err := db.Notices(context.Background(), f.projectID, true)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range staleOpen {
		if notice.Kind == "sandbox_missing" && notice.TaskID == f.task(t, "t1").ID {
			t.Fatalf("verified checkout did not resolve its stale missing-Sandbox Notice: %#v", notice)
		}
	}

	// The same Task must still produce a Notice if its checkout genuinely goes
	// missing later. This catches a false-positive Notice masking real loss.
	f.fail(t, "t1")
	f.exitWorker(t, "t1")
	if _, err := exec.Command(realGit, "-C", f.repo, "worktree", "remove", "--force", "--force", blockedPath).CombinedOutput(); err != nil {
		t.Fatalf("remove fixture checkout to reproduce a later real loss: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blockedPath, ".git")); !os.IsNotExist(err) {
		t.Fatalf("fixture worktree metadata still exists after removal: %v", err)
	}
	// Start another real Task to run Sandbox reconciliation again. `list`
	// reports the recorded state but does not perform this recovery pass.
	fifth, fifthOutput := startCommand("concurrent-5")
	if err := fifth.Run(); err != nil {
		t.Fatalf("start Task after genuine checkout loss: %v\n%s", err, fifthOutput.String())
	}
	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	notices, err = db.Notices(context.Background(), f.projectID, true)
	if err != nil {
		t.Fatal(err)
	}
	missingNotices := 0
	for _, notice := range notices {
		if notice.TaskID == f.task(t, "t1").ID && notice.Kind == "sandbox_missing" {
			missingNotices++
		}
	}
	if missingNotices != 1 {
		t.Fatalf("later real missing checkout should create exactly one Notice, got %d: %#v", missingNotices, notices)
	}
	if !strings.Contains(firstOutput.String(), "task: t1") {
		t.Fatalf("first start output omitted its Task: %s", firstOutput.String())
	}
}
