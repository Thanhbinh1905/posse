package app

import (
	"context"
	"errors"
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
	binDir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "gb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })
	if err := os.Symlink("/usr/bin/true", filepath.Join(binDir, "greenhouse")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

func TestGreenhouseReviewRefreshesTargetAfterTaskBranchRefresh(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateWorking)
	freezeCILoopWithReview(t, f, 0, 0, "full-pipeline")
	plan, err := f.task.AdmittedPlan()
	if err != nil {
		t.Fatal(err)
	}
	plan.ReviewProvider = store.GreenhouseReviewProvider
	plan.ValidationLoops.ReviewParallel = "greenhouse"
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("review_provider = \"greenhouse\"\n[review_loop]\ngreenhouse_timeout = \"1s\"\n[defaults]\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTarget, err := gitOutput(context.Background(), f.repo, "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var refreshedTarget string
	f.service.greenhouseRound = func(ctx context.Context, round greenhouse.Round) (greenhouse.RoundResult, error) {
		calls++
		if calls == 1 {
			refreshedTarget = f.advanceTarget(t, false, true)
			largeFile := filepath.Join(f.root, "target-advance", "large-target-change.txt")
			if err := os.WriteFile(largeFile, []byte(strings.Repeat("large target change\n", 5000)), 0o600); err != nil {
				return greenhouse.RoundResult{}, err
			}
			gitTest(t, filepath.Join(f.root, "target-advance"), "add", "large-target-change.txt")
			gitTest(t, filepath.Join(f.root, "target-advance"), "commit", "-m", "add large target change")
			refreshedTarget = strings.TrimSpace(gitTest(t, filepath.Join(f.root, "target-advance"), "rev-parse", "HEAD"))
			gitTest(t, filepath.Join(f.root, "target-advance"), "push", "origin", "HEAD:refs/heads/main")
			gitTest(t, f.task.WorktreePath, "fetch", "origin", "main")
			gitTest(t, f.task.WorktreePath, "merge", "--no-edit", "origin/main")
			if round.Target != oldTarget {
				t.Errorf("initial Greenhouse target = %s, want %s", round.Target, oldTarget)
			}
		} else if round.Target != refreshedTarget {
			t.Errorf("Greenhouse request %d target = %s, want refreshed target %s", calls, round.Target, refreshedTarget)
		}
		return greenhouse.RoundResult{Outcome: greenhouse.Stale}, nil
	}
	binDir := t.TempDir()
	if err := os.Symlink("/usr/bin/true", filepath.Join(binDir, "greenhouse")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code, output, stderr := f.run("_greenhouse", "review", "t1"); code != 0 {
		t.Fatalf("Greenhouse review after target refresh: %d %s %s", code, output, stderr)
	}
	if calls != greenhouseStaleRounds {
		t.Fatalf("Greenhouse request count = %d, want %d", calls, greenhouseStaleRounds)
	}
}

func TestGreenhouseSizeErrorsRaiseNeedsDecision(t *testing.T) {
	for _, tc := range []struct {
		name, reason, measured string
	}{
		{name: "intent", reason: "intent_too_long: 8193 bytes exceeds 8192", measured: "8193 bytes"},
		{name: "prompt", reason: "prompt_exceeds_context (12000 tokens > 8192)", measured: "12000 tokens > 8192"},
		{name: "reviewer", reason: "context_overflow (12000 tokens > 8192)", measured: "12000 tokens > 8192"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if err := f.db.SetProjectLead(context.Background(), f.project.ID, "fixture", "lead-pane", "lead"); err != nil {
				t.Fatal(err)
			}
			f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			f.service.greenhouseRound = func(context.Context, greenhouse.Round) (greenhouse.RoundResult, error) {
				calls++
				return greenhouse.RoundResult{}, errors.New(tc.reason)
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
			if code, output, stderr := f.run("_greenhouse", "review", "t1"); code == 0 {
				t.Fatalf("size error unexpectedly succeeded: %s %s", output, stderr)
			}
			if calls != 1 {
				t.Fatalf("Greenhouse called %d times, want one", calls)
			}
			notices, err := f.db.Notices(context.Background(), f.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(notices) != 1 || notices[0].Kind != "needs_decision" || !strings.Contains(notices[0].Summary, tc.reason) || !strings.Contains(notices[0].Summary, tc.measured) {
				t.Fatalf("size-error Notices = %+v, want one needs_decision naming %q and %q", notices, tc.reason, tc.measured)
			}
		})
	}
}

func TestGateModeGreenhouseUsageIsPersisted(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateWorking)
	cost := 0.004
	f.service.greenhouseRound = func(context.Context, greenhouse.Round) (greenhouse.RoundResult, error) {
		return greenhouse.RoundResult{Outcome: greenhouse.Stale, Result: &greenhouse.HostResult{Usage: &greenhouse.TokenUsage{InputTokens: 10, OutputTokens: 4, CachedInputTokens: 2, CacheWriteTokens: 1, CostUSD: &cost}}}, nil
	}
	if code, output, stderr := f.run("_greenhouse", "review", "t1"); code != 0 {
		t.Fatalf("Greenhouse review: %d %s %s", code, output, stderr)
	}
	total, err := f.db.TaskUsageTotal(context.Background(), f.task.ID)
	if err != nil || !total.Known || !total.KnownCost || total.Input != 30 || total.Output != 12 || total.CacheRead != 6 || total.CacheWrite != 3 || total.CostUSD == nil || *total.CostUSD != cost*3 {
		t.Fatalf("gate-mode review usage = %+v, err=%v", total, err)
	}
}

func TestInconclusiveQuotaFailureIsRecordedAndBackedOff(t *testing.T) {
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
	f.service.greenhouseRound = func(context.Context, greenhouse.Round) (greenhouse.RoundResult, error) {
		return greenhouse.RoundResult{}, &greenhouse.TerminalReviewError{Status: "inconclusive", Reason: "quota exhausted"}
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
	if code, output, stderr := f.run("_greenhouse", "review", "t1"); code != 0 {
		t.Fatalf("inconclusive quota review: %d %s %s", code, output, stderr)
	}
	requests, err := f.db.GreenhouseReviewRequests(context.Background(), f.project.ID)
	if err != nil || len(requests) != 1 || requests[0].Outcome != "refused" {
		t.Fatalf("inconclusive quota requests = %+v, %v; want one refused request", requests, err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || len(notices) != 0 {
		t.Fatalf("inconclusive quota notices = %+v, %v; want no failure Notice", notices, err)
	}
	starts := 0
	previous := selfExecCommand
	selfExecCommand = func(...string) (*exec.Cmd, error) {
		starts++
		return exec.Command("true"), nil
	}
	t.Cleanup(func() { selfExecCommand = previous })
	started := false
	if err := f.service.startParallelGreenhouseReviewsTracked(context.Background(), f.db, f.home, f.project, f.task, []store.ReviewTarget{{Repository: f.project.Name, Commit: head}}, &started, false); err != nil {
		t.Fatal(err)
	}
	if started || starts != 0 {
		t.Fatalf("automatic review restarted before quota retry backoff: started=%t, children=%d", started, starts)
	}
	notices, err = f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil || countNoticeKind(notices, "needs_decision") != 0 {
		t.Fatalf("inconclusive quota needs_decision Notices = %+v, %v; want none", notices, err)
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
