//go:build e2e && linux

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPoolRepairRecoversBrokenSandboxWithoutLosingWork(t *testing.T) {
	f := newWorkerTabsFixture(t)
	trackedDir := filepath.Join(f.repo, "docs")
	if err := os.MkdirAll(trackedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trackedDir, "spec.md"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", "docs/spec.md")
	gitTest(t, f.env, f.repo, "commit", "-m", "add repair fixture")

	task := f.start(t, "t1", "Repair broken Sandbox", "repair-broken-sandbox")
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")
	trackedPath := filepath.Join(task.WorktreePath, "docs", "spec.md")
	if err := os.WriteFile(trackedPath, []byte("recoverable tracked work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	untrackedPath := filepath.Join(task.WorktreePath, "untracked-evidence.txt")
	if err := os.WriteFile(untrackedPath, []byte("recoverable untracked work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rw, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.BreakSandbox(context.Background(), task.SandboxID, task.ID); err != nil {
		_ = rw.Close()
		t.Fatalf("set up a broken Sandbox: %v", err)
	}
	_ = rw.Close()
	if taskAfterBreak := f.task(t, "t1"); taskAfterBreak.SandboxID != 0 {
		t.Fatalf("broken Sandbox remained attached to Task %d", taskAfterBreak.SandboxID)
	}
	if _, err := f.client.Call(context.Background(), "pane.close", map[string]any{"pane_id": task.PaneID}); err != nil {
		t.Fatalf("close settled Worker pane: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		snapshot, err := f.client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID == task.PaneID {
				return false
			}
		}
		return true
	}) {
		t.Fatal("settled Worker pane remained open")
	}
	rw, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.Transition(context.Background(), task.ID, store.StateReported, store.StateTornDown, "cli", "E2E completes Teardown after the Sandbox was quarantined"); err != nil {
		_ = rw.Close()
		t.Fatalf("complete Task Teardown state: %v", err)
	}
	_ = rw.Close()
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "broken" || sandboxes[0].TaskID != 0 {
		t.Fatalf("broken Sandbox fixture was not quarantined: %#v %v", sandboxes, err)
	}

	foreign := exec.Command("sleep", "60")
	foreign.Dir, foreign.Env = task.WorktreePath, f.env
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if foreign.Process != nil {
			_ = foreign.Process.Kill()
			_ = foreign.Wait()
		}
	}()
	if !waitForCondition(5*time.Second, func() bool {
		bootID, startTime, err := store.ProcessIdentityForPID(foreign.Process.Pid)
		return err == nil && bootID != "" && startTime != ""
	}) {
		t.Fatal("foreign process did not start in the broken Sandbox")
	}
	repair := exec.Command(f.binary, "pool", "repair", "sandbox-1")
	repair.Dir, repair.Env = f.repo, f.leadEnv
	repairOutput, repairErr := repair.CombinedOutput()
	if repairErr == nil || !strings.Contains(string(repairOutput), "sandbox_process") {
		t.Fatalf("repair should refuse a foreign process without signalling it: err=%v output=%s", repairErr, repairOutput)
	}
	if _, _, err := store.ProcessIdentityForPID(foreign.Process.Pid); err != nil {
		t.Fatalf("repair signalled the foreign Sandbox process: %v", err)
	}
	if contents, err := os.ReadFile(trackedPath); err != nil || string(contents) != "recoverable tracked work\n" {
		t.Fatalf("refused repair changed tracked work: %q %v", contents, err)
	}
	if err := foreign.Process.Kill(); err != nil && err != syscall.ESRCH {
		t.Fatalf("stop test-owned foreign process: %v", err)
	}
	_ = foreign.Wait()
	foreign.Process = nil
	gitMetadataPath := filepath.Join(task.WorktreePath, ".git")
	gitMetadata, err := os.ReadFile(gitMetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitMetadataPath, []byte("gitdir: /missing/posse-repair-probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repair = exec.Command(f.binary, "pool", "repair", "sandbox-1")
	repair.Dir, repair.Env = f.repo, f.leadEnv
	repairOutput, repairErr = repair.CombinedOutput()
	if repairErr == nil || !strings.Contains(string(repairOutput), "sandbox_repair_git_metadata") {
		t.Fatalf("repair should classify broken Git metadata and keep the Sandbox quarantined: err=%v output=%s", repairErr, repairOutput)
	}
	if err := os.WriteFile(gitMetadataPath, gitMetadata, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err = db.Sandboxes(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "broken" {
		t.Fatalf("Git metadata failure changed the quarantined Sandbox state: %#v %v", sandboxes, err)
	}

	repair = exec.Command(f.binary, "pool", "repair", "sandbox-1")
	repair.Dir, repair.Env = f.repo, f.leadEnv
	repairOutput, repairErr = repair.CombinedOutput()
	if repairErr != nil || !strings.Contains(string(repairOutput), "idle") {
		t.Fatalf("repair cleanly recovered the broken Sandbox: err=%v output=%s", repairErr, repairOutput)
	}
	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err = db.Sandboxes(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("repaired Sandbox state = %#v %v", sandboxes, err)
	}
	if status := strings.TrimSpace(gitTest(t, f.env, task.WorktreePath, "status", "--porcelain")); status != "" {
		t.Fatalf("repaired Sandbox is not clean: %s", status)
	}
	registered := false
	for _, block := range strings.Split(gitTest(t, f.env, f.repo, "worktree", "list", "--porcelain"), "\n\n") {
		if strings.Contains(block, "worktree "+task.WorktreePath) {
			registered = true
			if strings.Contains(block, "\nlocked") {
				t.Fatalf("repaired Sandbox retained its Git worktree lock: %s", block)
			}
		}
	}
	if !registered {
		t.Fatalf("repaired Sandbox is missing from Git worktree registrations")
	}
	for _, saved := range []struct {
		path string
		want string
	}{
		{filepath.Join(f.home, "projects", "shop", "tasks", "t1", "docs", "spec.md"), "recoverable tracked work\n"},
		{filepath.Join(f.home, "projects", "shop", "tasks", "t1", "untracked-evidence.txt"), "recoverable untracked work\n"},
	} {
		if contents, err := os.ReadFile(saved.path); err != nil || string(contents) != saved.want {
			t.Fatalf("repair did not preserve work at %s: %q %v", saved.path, contents, err)
		}
	}
}
