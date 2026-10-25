//go:build e2e && linux

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func runPosseExpectingFailure(t *testing.T, f *workerTabsFixture, dir string, env []string, want string, args ...string) {
	t.Helper()
	command := exec.Command(f.binary, args...)
	command.Dir, command.Env = dir, env
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), want) {
		t.Fatalf("posse %v: want failure containing %q, got err=%v output=%s", args, want, err, output)
	}
}

func workerSignalEnv(f *workerTabsFixture, task store.Task) []string {
	return setEnv(setEnv(f.env, "HERDR_ENV", "1"), "HERDR_PANE_ID", task.PaneID)
}

// TestNoChangeTaskFinishesAndTearsDownWithoutDiscard covers a Change Task whose
// correct outcome is "already satisfied": the Worker finishes with evidence and
// the Lead tears it down without a User-approved discard.
func TestNoChangeTaskFinishesAndTearsDownWithoutDiscard(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Already satisfied", "already-satisfied")
	workerEnv := workerSignalEnv(f, task)

	runPosseExpectingFailure(t, f, task.WorktreePath, workerEnv, "--no-change", "signal", "done", "Already fixed on main")
	runPosse(t, f.binary, task.WorktreePath, workerEnv, "signal", "done", "--no-change", "Focused test passes on main: go test ./... -> ok")
	if done := f.task(t, "t1"); done.State != store.StateDone {
		t.Fatalf("no-change done Signal left Task in %s", done.State)
	}
	late := filepath.Join(task.WorktreePath, "late.txt")
	if err := os.WriteFile(late, []byte("late edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "teardown_refused", "teardown", "t1")
	if err := os.Remove(late); err != nil {
		t.Fatal(err)
	}
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("teardown without --discard did not tear down the no-change Task: %s", output)
	}
	if torn := f.task(t, "t1"); torn.State != store.StateTornDown {
		t.Fatalf("teardown left Task in %s", torn.State)
	}
	if out, err := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch).CombinedOutput(); err == nil {
		t.Fatalf("empty Task branch survived teardown: %s", out)
	}
}

// TestTeardownStillRefusesUnapprovedUnmergedWork keeps the safety rule: commits
// beyond base or a dirty Sandbox still need a User-approved discard.
func TestTeardownStillRefusesUnapprovedUnmergedWork(t *testing.T) {
	for _, name := range []string{"committed", "dirty", "untracked"} {
		t.Run(name, func(t *testing.T) {
			f := newWorkerTabsFixture(t)
			task := f.start(t, "t1", "Has work", "has-work")
			workerEnv := workerSignalEnv(f, task)
			file := filepath.Join(task.WorktreePath, "work.txt")
			switch name {
			case "committed":
				if err := os.WriteFile(file, []byte("work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, f.env, task.WorktreePath, "add", "work.txt")
				gitTest(t, f.env, task.WorktreePath, "commit", "-m", "work")
				runPosseExpectingFailure(t, f, task.WorktreePath, workerEnv, "signal_invalid", "signal", "done", "--no-change", "claims no change")
				command := exec.Command(f.binary, "signal", "done", "Work complete")
				command.Dir, command.Env = task.WorktreePath, workerEnv
				if out, err := command.CombinedOutput(); err != nil || strings.Contains(string(out), "--no-change") {
					t.Fatalf("committed work must keep commit guidance: err=%v output=%s", err, out)
				}
			case "dirty":
				if err := os.WriteFile(filepath.Join(task.WorktreePath, "README.md"), []byte("edited\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runPosseExpectingFailure(t, f, task.WorktreePath, workerEnv, "signal_invalid", "signal", "done", "--no-change", "claims no change")
				command := exec.Command(f.binary, "signal", "done", "Dirty")
				command.Dir, command.Env = task.WorktreePath, workerEnv
				if out, err := command.CombinedOutput(); err == nil || strings.Contains(string(out), "posse signal done <note> --no-change") {
					t.Fatalf("dirty worktree must keep commit guidance: err=%v output=%s", err, out)
				}
			case "untracked":
				if err := os.WriteFile(file, []byte("work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runPosseExpectingFailure(t, f, task.WorktreePath, workerEnv, "signal_invalid", "signal", "done", "--no-change", "claims no change")
			}
			runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "teardown_refused", "teardown", "t1")
			if current := f.task(t, "t1"); current.State == store.StateTornDown {
				t.Fatalf("teardown without approval tore down a Task with unmerged work")
			}
			f.assertAlive(t, f.snapshot(t), task.PaneID)
		})
	}
}
