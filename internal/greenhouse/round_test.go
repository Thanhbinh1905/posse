package greenhouse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSettledInspectionKeepsPendingRecord(t *testing.T) {
	worktree := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		command := exec.Command("git", args...)
		command.Dir = worktree
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "file"}, {"commit", "-q", "-m", "test"}} {
		command := exec.Command("git", args...)
		command.Dir = worktree
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	head, err := worktreeHead(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(t.TempDir(), "review.pending")
	if err := os.WriteFile(pending, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, ok, err := settledInspection(context.Background(), Round{Worktree: worktree}, "shop/t1/shop/r2", response{
		RunID: "run", HostResult: &HostResult{Grant: "shop/t1/shop/r2", Head: head, Current: true, Verdict: "findings"},
	})
	if err != nil || !ok || result.Outcome != Settled {
		t.Fatalf("settled inspection = %+v, %t, %v", result, ok, err)
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("settled inspection removed pending request before the caller records its Verdict: %v", err)
	}
}

func TestNextGrantNumberIncludesSettledReviewUsage(t *testing.T) {
	got := nextGrantNumber(&run{HostGrants: []string{"shop/t1/shop/r1"}, ReviewUsed: 2})
	if got != 3 {
		t.Fatalf("next grant number = %d, want 3 after two settled reviews", got)
	}
}

func TestTerminalReviewStatus(t *testing.T) {
	for _, status := range []string{"cancelled", "failed", "unconfigured", "refused"} {
		if !terminalReviewStatus(status) {
			t.Errorf("terminalReviewStatus(%q) = false, want true", status)
		}
	}
	for _, status := range []string{"running", "cancelling", "paused"} {
		if terminalReviewStatus(status) {
			t.Errorf("terminalReviewStatus(%q) = true, want false", status)
		}
	}
}
