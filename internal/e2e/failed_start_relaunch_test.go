//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func recoveryDecision(t *testing.T, f *intentProjectFixture) store.Decision {
	t.Helper()
	var decisions []store.Decision
	if err := f.openDB(t, func(db *store.DB) error {
		var err error
		decisions, err = db.Decisions(context.Background(), f.project.ID, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Kind != "recovery" {
		t.Fatalf("want one pending recovery Decision, got %#v", decisions)
	}
	return decisions[0]
}

// A start that failed before launch released its Sandbox. The Task never ran,
// so the recovery Decision offers relaunch and applying it starts a Worker on
// a fresh Sandbox.
func TestFailedStartDecisionRelaunchAcquiresFreshSandbox(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "failed-start-relaunch")
	brief := filepath.Join(f.root, "brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Failed start\ndone_when: the Worker launches\n---\nCreate a small change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run(t, "start:after:task.create", "start", "--brief", brief, "--name", "failed-start"); code != 86 {
		t.Fatalf("start did not crash after task creation: %d %s", code, out)
	}
	if code, out := f.run(t, "", "list"); code != 0 {
		t.Fatalf("list: %d %s", code, out)
	}
	var task store.Task
	if err := f.openDB(t, func(db *store.DB) error {
		var err error
		task, err = db.Task(context.Background(), f.project.ID, "t1")
		if err != nil {
			return err
		}
		sandboxes, err := db.Sandboxes(context.Background(), f.project.ID)
		if err == nil && len(sandboxes) != 0 {
			t.Errorf("failed start still holds Sandboxes: %#v", sandboxes)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if task.State != store.StateFailed {
		t.Fatalf("Task state = %s, want failed", task.State)
	}
	decision := recoveryDecision(t, f)
	if !slices.Contains(decision.Options, "relaunch") {
		t.Fatalf("never-started recovery Decision omitted relaunch: %#v", decision.Options)
	}
	id := strconv.FormatInt(decision.ID, 10)
	if code, out := f.run(t, "", "decide", id, "relaunch", "--user-approved", "relaunch it"); code != 0 {
		t.Fatalf("decide: %d %s", code, out)
	}
	if code, out := f.run(t, "", "apply", id); code != 0 || !strings.Contains(out, "working") {
		t.Fatalf("apply relaunch: %d %s", code, out)
	}
	if err := f.openDB(t, func(db *store.DB) error {
		after, err := db.Task(context.Background(), f.project.ID, "t1")
		if err != nil {
			return err
		}
		sandbox, err := db.SandboxByTask(context.Background(), after.ID)
		if err != nil {
			return err
		}
		if after.State != store.StateWorking || after.Launches != 1 || after.PaneID == "" || sandbox.State != "held" || after.WorktreePath != sandbox.Path {
			t.Errorf("relaunched Task = state %s launches %d pane %q worktree %q sandbox %#v", after.State, after.Launches, after.PaneID, after.WorktreePath, sandbox)
		}
		intents, err := db.Intents(context.Background(), f.project.ID)
		if err == nil && len(intents) != 0 {
			t.Errorf("relaunch left %d intent(s)", len(intents))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.home, "projects", f.project.Name, "tasks", "t1", "launch.md")); err != nil {
		t.Fatalf("relaunch wrote no launch Brief: %v", err)
	}
}

// A Task that already ran and lost its Sandbox has work that relaunch cannot
// restore: relaunch refuses clearly and the Decision does not offer it.
func TestFailedTaskWithPriorWorkAndNoSandboxCannotRelaunch(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "prior-work-no-sandbox")
	if _, err := f.addChangeTask(t, store.StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := f.openDB(t, func(db *store.DB) error {
		sandbox, err := db.SandboxByTask(context.Background(), f.taskDBID)
		if err != nil {
			return err
		}
		if err := db.BeginSandboxRelease(context.Background(), sandbox.ID, f.taskDBID); err != nil {
			return err
		}
		if err := db.FinishSandboxRelease(context.Background(), sandbox.ID, f.taskDBID); err != nil {
			return err
		}
		_, err = db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, TaskID: f.taskDBID, Kind: "task_failed", Summary: "t1 failed", DataJSON: `{}`})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run(t, "", "list"); code != 0 {
		t.Fatalf("list: %d %s", code, out)
	}
	decision := recoveryDecision(t, f)
	if slices.Contains(decision.Options, "relaunch") || !slices.Contains(decision.Options, "discard") {
		t.Fatalf("Decision options = %#v, want discard without relaunch", decision.Options)
	}
	code, out := f.run(t, "", "relaunch", "t1")
	if code == 0 || !strings.Contains(out, "relaunch_sandbox_missing") || !strings.Contains(out, "prior work") {
		t.Fatalf("relaunch should refuse with a clear error: %d %s", code, out)
	}
}
