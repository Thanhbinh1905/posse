package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdmittedExplicitBaseDoesNotFollowLaterBranchMoves(t *testing.T) {
	f := newStartFixture(t)
	gitTest(t, f.repo, "branch", "release/next")
	initial := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "release/next"))
	text := "---\ntype: change\ntitle: Launch probe\ndone_when: checked\nbase: release/next\ntarget: release/next\n---\n"
	if err := os.WriteFile(f.brief, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output := f.start(t); code != 0 {
		t.Fatalf("start: %d %s", code, output)
	}
	task, _ := f.onlyTask(t)
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || plan.Execution.BaseCommit != initial || task.BaseRef != initial {
		t.Fatalf("base not pinned: %#v %v", plan, err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "later.txt"), []byte("later base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "add", "later.txt")
	gitTest(t, f.repo, "commit", "-m", "later base")
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "task.txt"), []byte("Task change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, task.WorktreePath, "add", "task.txt")
	gitTest(t, task.WorktreePath, "commit", "-m", "Task change")
	gitTest(t, f.repo, "branch", "-f", "release/next", task.Branch)
	if err := validateChangeSignal(context.Background(), task); err != nil {
		t.Fatalf("moved base changed completion proof: %v", err)
	}
	base, err := taskCheckoutBase(context.Background(), f.repo, task, "", task.BaseRef)
	if err != nil || base != initial {
		t.Fatalf("base followed moved branch: %s %v", base, err)
	}
	// An unavailable pinned commit must not silently fall back to the live ref.
	plan.Execution.BaseCommit = strings.Repeat("1", 40)
	task.PlanJSON, err = plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskCheckoutBase(context.Background(), f.repo, task, "", task.BaseRef); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("missing pinned base was substituted: %v", err)
	}
}
