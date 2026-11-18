package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReviewStartPassesPinnedHeadAfterSandboxOwnershipRace(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	var err error
	pooled := filepath.Join(f.home, "pool", "shop", "sandbox-1")
	if err := os.MkdirAll(filepath.Dir(pooled), 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "worktree", "move", f.worktree, pooled)
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET worktree_path=? WHERE id=?`, pooled, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.worktree, f.task.WorktreePath = pooled, pooled
	attachPRFixtureSandbox(t, f)
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("review_provider = \"greenhouse\"\n[defaults]\nmerge_mode = \"pr\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "refs/heads/"+f.task.Branch))
	previous := selfExecCommand
	selfExecCommand = func(args ...string) (*exec.Cmd, error) {
		foundHead := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--head" {
				foundHead = true
				if args[i+1] != head {
					t.Errorf("Greenhouse child head = %s, want Task branch head %s", args[i+1], head)
				}
			}
		}
		if !foundHead {
			t.Errorf("Greenhouse child arguments omit pinned Task head: %v", args)
		}
		if _, err := releaseSandbox(context.Background(), f.db, f.project, f.task, "warm", false); err != nil {
			return nil, err
		}
		otherID, err := f.db.CreateTask(context.Background(), f.project.ID, store.Task{Seq: 2, Type: "change", Title: "reuse Sandbox", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/t2", BaseRef: "main"})
		if err != nil {
			return nil, err
		}
		other := store.Task{ID: otherID, ProjectID: f.project.ID, Seq: 2, Type: "change", Branch: "posse/t2", BaseRef: "main"}
		reused, err := acquireSandbox(context.Background(), f.db, f.project, other, f.home, "warm", nil)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(reused.Path, "other-task.txt"), []byte("other Task head\n"), 0o600); err != nil {
			return nil, err
		}
		gitTest(t, reused.Path, "add", "other-task.txt")
		gitTest(t, reused.Path, "commit", "-m", "other Task head")
		return exec.Command("true"), nil
	}
	t.Cleanup(func() { selfExecCommand = previous })

	if code, output, stderr := f.run("review", "start", "t1"); code != 0 {
		t.Fatalf("review start across Sandbox reuse: %d %s %s", code, output, stderr)
	}
	roundDir := filepath.Join(f.home, "projects", f.project.Name, "tasks", "t1", "greenhouse")
	pendingPath := filepath.Join(roundDir, f.project.Name+".pending.json")
	if err := os.MkdirAll(roundDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, []byte(`{"grant":"shop/t1/shop/r1","request":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ownerPath := parallelReviewOwnerPath(roundDir, f.project.Name)
	if err := writeParallelReviewOwner(ownerPath, parallelReviewOwner{PID: os.Getpid(), Head: head, RequestedAt: 123}); err != nil {
		t.Fatal(err)
	}
	var reviewedHead string
	f.service.greenhouseRound = func(ctx context.Context, round greenhouse.Round) (greenhouse.RoundResult, error) {
		var err error
		reviewedHead, err = gitOutput(ctx, round.Worktree, "rev-parse", "HEAD")
		return greenhouse.RoundResult{Outcome: greenhouse.Stale}, err
	}
	if code, output, stderr := f.run("_greenhouse", "review", "t1", "--parallel-owner", ownerPath); code != 0 {
		t.Fatalf("run pinned Greenhouse review: %d %s %s", code, output, stderr)
	}
	if reviewedHead != head {
		t.Fatalf("Greenhouse reviewed %s after Sandbox reuse, want Task head %s", reviewedHead, head)
	}
}

func TestGreenhouseReviewRecordsCancelledTerminalRunAsFailure(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateWorking)
	freezeCILoopWithReview(t, f, 0, 0, "full-pipeline")
	plan, err := f.task.AdmittedPlan()
	if err != nil {
		t.Fatal(err)
	}
	plan.ReviewProvider = store.GreenhouseReviewProvider
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=? AND revision=1`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectLead(context.Background(), f.project.ID, "fixture", "lead-pane", "lead"); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	head, err := gitOutput(context.Background(), f.task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	grant := "shop/t1/shop/r1"
	reason := "executor exceeded its 10-minute limit"
	f.service.greenhouseRound = func(context.Context, greenhouse.Round) (greenhouse.RoundResult, error) {
		return greenhouse.RoundResult{Grant: grant, RunID: "cancelled-run"}, &greenhouse.TerminalReviewError{Status: "cancelled", Reason: reason}
	}
	binDir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "gb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })
	if err := os.Symlink("/usr/bin/true", filepath.Join(binDir, "greenhouse")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	code, output, stderr := f.run("_greenhouse", "review", "t1")
	if code == 0 {
		t.Fatalf("cancelled Greenhouse review unexpectedly succeeded: %s %s", output, stderr)
	}
	requests, err := f.db.GreenhouseReviewRequests(context.Background(), f.project.ID)
	if err != nil || len(requests) != 1 || requests[0].Outcome != "failed" || requests[0].Grant != grant || requests[0].RequestedHead != head {
		t.Fatalf("cancelled Greenhouse request ledger = %+v, %v; CLI exit=%d stdout=%s stderr=%s", requests, err, code, output, stderr)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Kind != "greenhouse_review_failed" || !strings.Contains(notices[0].Summary, `status "cancelled"`) || !strings.Contains(notices[0].Summary, reason) {
		t.Fatalf("cancelled Greenhouse failure Notice = %+v", notices)
	}
}
