package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Every observed merged PR must finish Teardown or retain exactly one
// deduplicated, visible reason. A terminal Worker state never drops the watch.
func TestMergedPRNeverSilentlySkipsTask(t *testing.T) {
	states := []store.State{store.StateWorking, store.StateNeedsDecision, store.StateBlocked, store.StateStalled, store.StateFailed, store.StateLost, store.StateDone, store.StateMerging}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			f := newPRMergeFixture(t, "pr", store.StateWorking)
			defer f.db.Close()
			ctx := context.Background()
			if state == store.StateMerging {
				if err := f.db.Transition(ctx, f.task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
					t.Fatal(err)
				}
				if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateMerging, "cli", "merging"); err != nil {
					t.Fatal(err)
				}
			} else if state != store.StateWorking {
				source := "cli"
				if state == store.StateDone || state == store.StateFailed || state == store.StateNeedsDecision {
					source = "worker"
				}
				if state == store.StateBlocked {
					source = "herdr"
				}
				if err := f.db.Transition(ctx, f.task.ID, store.StateWorking, state, source, "test"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.db.UpdateTaskMerge(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
			cfg, err := config.Load(f.home, f.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for tick := 0; tick < 2; tick++ {
				if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
					t.Fatal(err)
				}
				if err := f.service.autoTeardownMergedTasks(ctx, f.db, f.project, cfg); err != nil {
					t.Fatal(err)
				}
			}
			task, err := f.db.Task(ctx, f.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			notices, err := f.db.Notices(ctx, f.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			reasons := countNoticeKind(notices, "teardown_incomplete") + countNoticeKind(notices, "pr_watch_failing")
			if task.State != store.StateTornDown && (task.State != store.StateMerged || reasons != 1) {
				t.Fatalf("merged PR silently skipped: state=%s reasons=%d notices=%#v", task.State, reasons, notices)
			}
			if countNoticeKind(notices, "pr_merged") != 1 {
				t.Fatalf("merged Notice count = %d", countNoticeKind(notices, "pr_merged"))
			}
		})
	}
}

func (fixture *prMergeFixture) advanceTarget(t *testing.T, conflicting, push bool) string {
	t.Helper()
	worktree := filepath.Join(fixture.root, "target-advance")
	gitTest(t, fixture.repo, "worktree", "add", "-b", "target-advance", worktree, "refs/heads/main")
	path, contents := "target-only.txt", "target update\n"
	if conflicting {
		path, contents = "README.md", "target README update\n"
	}
	if err := os.WriteFile(filepath.Join(worktree, path), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", path)
	gitTest(t, worktree, "commit", "-m", "advance target")
	tip := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD"))
	if push {
		gitTest(t, worktree, "push", "origin", tip+":refs/heads/main")
	}
	return tip
}

type prMergeTestProgress struct{}

func (prMergeTestProgress) ReadPane(context.Context, herdr.Adapter, string, int) (string, error) {
	return "", nil
}

func (prMergeTestProgress) WorktreeFingerprint(context.Context, string, bool) (string, error) {
	return "", nil
}

func TestPRMergeIntentProtectsTaskBeforeProjectReconciliation(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	fixture.service.Progress = prMergeTestProgress{}
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, stderr)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}

	completed, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, status := range []string{"working", "idle"} {
		if _, err := fixture.db.ObserveCompletedWorker(context.Background(), completed, fixture.task.PaneID, "generation", status, now); err != nil {
			t.Fatalf("record completed Worker %s status: %v", status, err)
		}
		now = now.Add(time.Second)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	snapshot := fake.SnapshotValue
	snapshot.ServerStartedAt = "generation"
	for index := range snapshot.Panes {
		if snapshot.Panes[index].PaneID == fixture.task.PaneID {
			snapshot.Panes[index].AgentStatus = "working"
		}
	}
	fake.SnapshotValue = snapshot

	if code, output, stderr := fixture.run("merge", "t1", "--execute", "--user-approved", "User approved the PR merge"); code != 0 || !strings.Contains(output, "state: merging") {
		t.Fatalf("merge lost to a completed Worker observation: exit=%d output=%s error=%s", code, output, stderr)
	}
	current, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || current.State != store.StateMerging {
		t.Fatalf("merge changed state to %s after Worker observation: %v", current.State, err)
	}
}

func TestPRWatchBlocksStaleChecksAndQueuesTargetMergeForCleanAndConflictingBranches(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		conflicting bool
		mergeable   string
	}{
		{name: "behind but clean", mergeable: "MERGEABLE"},
		{name: "conflicting", conflicting: true, mergeable: "CONFLICTING"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			if scenario.conflicting {
				path := filepath.Join(fixture.worktree, "README.md")
				if err := os.WriteFile(path, []byte("Task README update\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, fixture.worktree, "add", "README.md")
				gitTest(t, fixture.worktree, "commit", "-m", "change README")
				fixture.headSHA = strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
			}
			if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
				t.Fatalf("open PR exit=%d output=%s error=%s", code, output, stderr)
			}
			targetTip := fixture.advanceTarget(t, scenario.conflicting, true)
			if err := os.WriteFile(fixture.ghLog, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", scenario.mergeable, "", fixture.headSHA)
			cfg, err := config.Load(fixture.home, fixture.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			queryLog, err := os.ReadFile(fixture.ghLog)
			if err != nil || strings.Contains(string(queryLog), "statusCheckRollup") {
				t.Fatalf("watcher queried final CI for a branch already known to be behind: query=%q err=%v", queryLog, err)
			}
			observation, err := fixture.db.LatestPRObservation(context.Background(), fixture.task.ID)
			if err != nil || decodeChecks(observation.Checks).State != "STALE" {
				t.Fatalf("stale CI result remained usable: observation=%#v err=%v", observation, err)
			}
			notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
			if err != nil || countNoticeKind(notices, "merge_ready") != 0 {
				t.Fatalf("stale PR was treated as merge-ready: notices=%#v err=%v", notices, err)
			}
			var body string
			var queued int
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT body FROM messages WHERE task_id=? AND status='queued' ORDER BY id DESC LIMIT 1`, fixture.task.ID).Scan(&body); err != nil {
				t.Fatalf("Posse did not queue a Worker freshness instruction: %v", err)
			}
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&queued); err != nil || queued != 1 {
				t.Fatalf("repeated watcher polls duplicated the freshness instruction: queued=%d err=%v", queued, err)
			}
			var expectedHead, kind string
			if err := fixture.db.QueryRowContext(context.Background(), `SELECT expected_base_head,kind FROM validation_fix_rounds WHERE ship_task_id=?`, fixture.task.ID).Scan(&expectedHead, &kind); err != nil || expectedHead != fixture.headSHA || kind != "freshness" {
				t.Fatalf("freshness instruction binding = %q kind=%q err=%v, want %q/freshness", expectedHead, kind, err, fixture.headSHA)
			}
			for _, required := range []string{targetTip, "origin/main", "Merge origin/main", "republish the updated head", "never rebase a pushed branch", "posse publish"} {
				if !strings.Contains(body, required) {
					t.Errorf("freshness instruction omitted %q: %s", required, body)
				}
			}
		})
	}
}

func TestPRWatchRaisesMergeReadyWhenTaskEntersMergeAfterPRIsGreen(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	ctx := context.Background()
	url := "https://github.com/acme/shop/pull/17"
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET autonomy_land='auto' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := fixture.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, output, stderr)
	}
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, url, ""); err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	poll() // Checks pass while the Worker is still working, so no Notice is due yet.
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil || task.State != store.StateWorking {
		t.Fatalf("publish changed the Task state before the done Signal: task=%#v err=%v", task, err)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "merge_ready") != 0 {
		t.Fatalf("working Task unexpectedly received merge_ready: notices=%#v err=%v", notices, err)
	}
	if code, output, stderr := fixture.run("signal", "done", "Worker completion summary", "--pr", url); code != 0 {
		t.Fatalf("done: %d %s %s", code, output, stderr)
	}
	poll() // The Task became done after its green PR observation was recorded.
	notices, err = fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "merge_ready") != 1 {
		t.Fatalf("green PR did not produce merge_ready when the Task became done: notices=%#v err=%v", notices, err)
	}
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("merge: %d %s %s", code, output, stderr)
	}
	task, err = fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil || task.State != store.StateMerging {
		t.Fatalf("merge did not move the Task to merging: task=%#v err=%v", task, err)
	}
	poll() // The unchanged green observation must not duplicate merge_ready.
	notices, err = fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "merge_ready") != 1 {
		t.Fatalf("repeated ready observation duplicated merge_ready after Merge: notices=%#v err=%v", notices, err)
	}
	poll()
	notices, err = fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "merge_ready") != 1 {
		t.Fatalf("repeated ready observations duplicated merge_ready: notices=%#v err=%v", notices, err)
	}
	if err := fixture.service.raiseNoticeDecisions(ctx, fixture.db, fixture.project); err != nil {
		t.Fatal(err)
	}
	decisions, err := fixture.db.Decisions(ctx, fixture.project.ID, true)
	if err != nil || len(decisions) != 0 {
		t.Fatalf("autonomy.merge=auto should emit merge_ready without creating a User Decision: decisions=%#v err=%v", decisions, err)
	}
}

func TestPRWatchRejectsTargetThatAdvancesDuringCIObservation(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, stderr)
	}
	targetTip := fixture.advanceTarget(t, false, false)
	t.Setenv("POSSE_TEST_GH_ADVANCE_SHA", targetTip)
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	remoteTip := strings.TrimSpace(gitTest(t, fixture.remote, "rev-parse", "refs/heads/main"))
	if remoteTip != targetTip {
		t.Fatalf("fake forge did not advance target during check observation: remote=%s want=%s", remoteTip, targetTip)
	}
	observation, err := fixture.db.LatestPRObservation(context.Background(), fixture.task.ID)
	if err != nil || decodeChecks(observation.Checks).State != "STALE" {
		t.Fatalf("CI observed before the target advanced was accepted: observation=%#v err=%v", observation, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "merge_ready") != 0 {
		t.Fatalf("target advance during CI produced merge-ready evidence: notices=%#v err=%v", notices, err)
	}
}

func TestPRWatchRecordsChecksReviewConflictAndMergeTransitions(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}

	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	poll()
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_checks_failed") != 1 {
		t.Fatalf("failed checks Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_checks_failed"), notices, err)
	}
	observation, err := fixture.db.LatestPRObservation(ctx, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	checks := decodeChecks(observation.Checks)
	if observation.HeadSHA != fixture.headSHA || checks.State != "FAILURE" || len(checks.Failures) != 1 || checks.Failures[0].Name != "unit" || checks.Failures[0].URL != "https://checks.example/unit" {
		t.Fatalf("failed check snapshot lost its name or URL: %#v %#v", observation, checks)
	}

	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	poll()
	if err := os.WriteFile(filepath.Join(fixture.worktree, "next.txt"), []byte("next head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", "next.txt")
	gitTest(t, fixture.worktree, "commit", "-m", "advance PR head")
	newHead := strings.TrimSpace(gitTest(t, fixture.worktree, "rev-parse", "HEAD"))
	gitTest(t, fixture.worktree, "push", "origin", "HEAD:refs/heads/posse/t1")
	fixture.setGHHead(t, newHead)
	fixture.setGraphQLState(t, "OPEN", "PENDING", "CHANGES_REQUESTED", "CONFLICTING", "", newHead)
	poll()
	notices, err = fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "merge_ready") != 1 || countNoticeKind(notices, "pr_changes_requested") != 1 || countNoticeKind(notices, "pr_conflict") != 1 {
		t.Fatalf("review and conflict notices were not recorded once: %#v, %v", notices, err)
	}
	observation, err = fixture.db.LatestPRObservation(ctx, fixture.task.ID)
	if err != nil || observation.HeadSHA != newHead || decodeChecks(observation.Checks).State != "PENDING" {
		t.Fatalf("new head did not reset check status: %#v, %v", observation, err)
	}

	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, newHead)
	poll()
	task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerged || task.MergedRef != fixture.headSHA {
		t.Fatalf("merged PR did not merge at the GitHub merge commit: %#v, %v", task, err)
	}
	notices, err = fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_merged") != 1 {
		t.Fatalf("merged Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_merged"), notices, err)
	}
	var rows int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pr_observations WHERE task_id=?`, task.ID).Scan(&rows); err != nil || rows != 4 {
		t.Fatalf("observation history has %d rows, want four distinct changes: %v", rows, err)
	}
}

func TestPRWatchRerunsCancelledCheckOnceThenQueuesOneCIFixRound(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	freezeCILoop(t, fixture, 3, 1)
	t.Setenv("POSSE_TEST_GH_CANCELLED", "1")
	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}

	poll()
	var reruns, fixes int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_rerun_requested'`, fixture.task.ID).Scan(&reruns); err != nil || reruns != 1 {
		t.Fatalf("cancelled check rerun notices=%d err=%v", reruns, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, fixture.task.ID).Scan(&fixes); err != nil || fixes != 0 {
		t.Fatalf("fix rounds after first cancellation=%d err=%v", fixes, err)
	}

	poll()
	poll()
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, fixture.task.ID).Scan(&fixes); err != nil || fixes != 1 {
		t.Fatalf("repeated cancelled observation fix rounds=%d err=%v, want one", fixes, err)
	}
	var queued, rerunNotices, queuedNotices int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued CI instructions=%d err=%v, want one", queued, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_rerun_requested'`, fixture.task.ID).Scan(&rerunNotices); err != nil || rerunNotices != 1 {
		t.Fatalf("rerun notices=%d err=%v, want one", rerunNotices, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_fix_queued'`, fixture.task.ID).Scan(&queuedNotices); err != nil || queuedNotices != 1 {
		t.Fatalf("CI fix notices=%d err=%v, want one", queuedNotices, err)
	}
	var body string
	if err := fixture.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"unit", "https://github.com/acme/shop/actions/runs/123/job/456", "unit failed log", "User intent (verbatim)"} {
		if !strings.Contains(body, required) {
			t.Errorf("CI fix instruction omitted %q: %s", required, body)
		}
	}
	ghLog, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(ghLog), "run rerun --failed 123 --repo acme/shop") != 1 || !strings.Contains(string(ghLog), "run view --job 456 --log-failed --repo acme/shop") {
		t.Fatalf("fake forge rerun/log commands = %q", ghLog)
	}
}

func TestGitLabPRWatchRetriesCancelledJobAndQueuesOneCIFixRound(t *testing.T) {
	fixture := gitlabFixture(t, store.StateDone)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://git.example.com/group/sub/shop/-/merge_requests/17", ""); err != nil {
		t.Fatal(err)
	}
	freezeCILoop(t, fixture, 3, 1)
	fixture.setGitLabState(t, "opened", "mergeable", "canceled", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	poll()
	poll()
	var reruns, fixes, queued int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_rerun_requested'`, fixture.task.ID).Scan(&reruns); err != nil || reruns != 1 {
		t.Fatalf("GitLab cancelled job rerun notices=%d err=%v", reruns, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, fixture.task.ID).Scan(&fixes); err != nil || fixes != 1 {
		t.Fatalf("GitLab CI fix rounds=%d err=%v", fixes, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("GitLab queued CI instructions=%d err=%v", queued, err)
	}
	var body string
	if err := fixture.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "unit failed log") || !strings.Contains(body, "https://git.example.com/group/sub/shop/-/jobs/77") {
		t.Fatalf("GitLab CI fix instruction omitted job evidence: %s", body)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(log), "ci retry 77 --repo https://git.example.com/group/sub/shop") != 1 {
		t.Fatalf("GitLab retry commands = %q", log)
	}
}

func TestPRWatchCIFixBudgetIgnoresHistoricalRepairsOutsideFullPipeline(t *testing.T) {
	for _, review := range []string{"none", "quick"} {
		t.Run(review, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			ctx := context.Background()
			if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			freezeCILoopWithReview(t, fixture, 1, 0, review)
			seedHistoricalValidationFixRounds(t, fixture, 3)
			fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
			cfg, err := config.Load(fixture.home, fixture.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			poll := func() {
				t.Helper()
				if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("POSSE_TEST_GH_FAILURE_NAME", "integration")
			t.Setenv("POSSE_TEST_GH_FAILURE_URL", "https://checks.example/integration")
			poll()

			var rounds, queued, exhausted int
			if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, fixture.task.ID).Scan(&rounds); err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_fix_queued'`, fixture.task.ID).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_fix_exhausted'`, fixture.task.ID).Scan(&exhausted); err != nil {
				t.Fatal(err)
			}
			if rounds != 1 || queued != 1 || exhausted != 0 {
				t.Fatalf("historical repairs reduced %s CI budget: rounds=%d queued=%d exhausted=%d", review, rounds, queued, exhausted)
			}
		})
	}
}

func TestPRWatchRaisesCIExhaustedOnceAfterFixBudget(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	freezeCILoop(t, fixture, 1, 0)
	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	t.Setenv("POSSE_TEST_GH_FAILURE_NAME", "integration")
	t.Setenv("POSSE_TEST_GH_FAILURE_URL", "https://checks.example/integration")
	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	poll()
	poll()
	var exhausted, queued int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_fix_exhausted'`, fixture.task.ID).Scan(&exhausted); err != nil || exhausted != 1 {
		t.Fatalf("CI exhausted notices=%d err=%v, want one", exhausted, err)
	}
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("CI fix messages=%d err=%v, want one", queued, err)
	}
}

func TestPRWatchDoesNotStartCILoopForStaleGatedHead(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	freezeCILoop(t, fixture, 3, 1)
	t.Setenv("POSSE_TEST_GH_CANCELLED", "1")
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET gated_sha=? WHERE id=?`, strings.Repeat("f", 40), fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`,
		`SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='ci_rerun_requested'`,
		`SELECT COUNT(*) FROM notices WHERE task_id=? AND kind IN ('ci_fix_queued','ci_fix_exhausted')`,
	} {
		var count int
		if err := fixture.db.QueryRowContext(ctx, query, fixture.task.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("stale observation side effect count=%d for %q err=%v", count, query, err)
		}
	}
}

func TestPRWatchMergesDoneTaskWhenItsPRIsMerged(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"finished\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sandbox := attachPRFixtureSandbox(t, fixture)
	prURL := "https://github.com/acme/shop/pull/17"
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	mergeCommit := strings.Repeat("9", 40)
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", mergeCommit, fixture.headSHA)
	fake, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the fake Herdr adapter")
	}
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	fixture.service.Herdr = &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
	removeFixtureTaskPane(fixture)

	if _, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project); err != nil {
		t.Fatalf("reconcile merged PR: %v", err)
	}
	task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown || task.MergedRef != mergeCommit {
		t.Fatalf("merged PR did not merge and follow auto_teardown=finished: %#v, %v", task, err)
	}
	releasedSandbox, err := fixture.db.SandboxByTask(ctx, task.ID)
	if err == nil {
		t.Fatalf("auto-teardown left Sandbox held: %#v", releasedSandbox)
	}
	sandboxes, err := fixture.db.Sandboxes(ctx, fixture.project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].ID != sandbox.ID || sandboxes[0].State != "idle" || sandboxes[0].TaskID != 0 {
		t.Fatalf("safe merged Task Sandbox was not released: %#v, %v", sandboxes, err)
	}
	observation, err := fixture.db.LatestPRObservation(ctx, task.ID)
	if err != nil || observation.State != "MERGED" || observation.MergeCommit != mergeCommit {
		t.Fatalf("merge observation did not record the merge commit: %#v, %v", observation, err)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_merged") != 1 {
		t.Fatalf("merged Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_merged"), notices, err)
	}
	transitions, err := fixture.db.TaskTransitions(ctx, task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundMerged := false
	for _, transition := range transitions {
		if transition.From == string(store.StateDone) && transition.To == string(store.StateMerged) {
			foundMerged = true
			break
		}
	}
	if !foundMerged {
		t.Fatalf("done Task did not transition through merged: %#v", transitions)
	}
}

func TestPRWatchPreservesMergedDoneTaskWithUnsafeWorktree(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *prMergeFixture)
	}{
		{
			name: "dirty worktree",
			edit: func(t *testing.T, fixture *prMergeFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.worktree, "late-change.txt"), []byte("preserve me\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "branch moved past merged head",
			edit: func(t *testing.T, fixture *prMergeFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.worktree, "follow-up.txt"), []byte("unreviewed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, fixture.worktree, "add", "follow-up.txt")
				gitTest(t, fixture.worktree, "commit", "-m", "unreviewed follow-up")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			sandbox := attachPRFixtureSandbox(t, fixture)
			if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(`[defaults]
merge_mode = "pr"
merge_method = "squash"
auto_teardown = "finished"
`), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.SetTaskGatedSHA(ctx, fixture.task.ID, fixture.headSHA); err != nil {
				t.Fatal(err)
			}
			tc.edit(t, fixture)
			fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, fixture.headSHA)
			fake, ok := fixture.service.Herdr.(*herdr.Fake)
			if !ok {
				t.Fatal("fixture does not use the fake Herdr adapter")
			}
			fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
			adapter := &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue}
			fixture.service.Herdr = adapter
			removeFixtureTaskPane(fixture)

			if _, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project); err != nil {
				t.Fatalf("reconcile merged PR: %v", err)
			}
			task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
			if err != nil || task.State != store.StateTornDown {
				t.Fatalf("merged Task was not torn down: %#v, %v", task, err)
			}
			if containsPane(adapter.currentSnapshot(), "w2:p1") {
				t.Fatal("merged Worker pane remained open")
			}
			if held, err := fixture.db.SandboxByTask(ctx, task.ID); err == nil {
				t.Fatalf("Sandbox %d remained held: %#v", sandbox.ID, held)
			}
			orphan := "refs/heads/posse/t1-orphan"
			if tc.name == "dirty worktree" {
				if content := gitTest(t, fixture.repo, "show", orphan+":late-change.txt"); content != "preserve me\n" {
					t.Fatalf("Orphan omitted untracked work: %q", content)
				}
			} else if commit := gitTest(t, fixture.repo, "log", "-1", "--format=%s", orphan); !strings.Contains(commit, "unreviewed follow-up") {
				t.Fatalf("Orphan omitted committed work: %q", commit)
			}
			decisions, err := fixture.db.Decisions(ctx, fixture.project.ID, true)
			if err != nil || len(decisions) != 1 || decisions[0].Kind != "orphan" {
				t.Fatalf("Orphan Decision: %#v %v", decisions, err)
			}
		})
	}
}

func TestPlainTeardownRefusesMergedPRWithLateWork(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	if code, out, stderr := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge: %d %s %s", code, out, stderr)
	}
	ctx := context.Background()
	merged, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, merged.GatedSHA)
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(f.worktree, "uncommitted-follow-up.txt")
	if err := os.WriteFile(work, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.run("teardown", "t1"); code == 0 {
		t.Fatal("Teardown succeeded without verifying the pane closed")
	}
	if content, err := os.ReadFile(work); err != nil || string(content) != "keep\n" {
		t.Fatalf("work was discarded before pane closure: %q %v", content, err)
	}
}

func TestPRMergePreservesWorkWrittenDuringPaneShutdown(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	sandbox := attachPRFixtureSandbox(t, f)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nauto_teardown = \"finished\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := f.db.UpdateTaskMerge(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", f.headSHA, f.headSHA)
	fake := f.service.Herdr.(*herdr.Fake)
	shell := exec.Command("sleep", "300")
	shell.Dir = f.worktree
	shell.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = shell.Process.Kill()
		_ = shell.Wait()
	}()
	shellIdentity, err := sandboxProcessExecutableIdentity(shell.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateTaskPaneShellIdentity(ctx, f.task.ID, shellIdentity); err != nil {
		t.Fatal(err)
	}
	worker := exec.Command("bash", "-c", "trap 'printf \"%s\\n\" \"preserve me\" > late-worker-work.txt; exit 0' TERM; while true; do read -t 0.1 || :; done")
	worker.Dir = f.worktree
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pids, err := sandboxProcessIDs(f.worktree)
		if err != nil {
			t.Fatal(err)
		}
		if len(pids) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start in Sandbox")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := f.db.UpdateTaskLaunch(ctx, f.task.ID, f.task.WorktreePath, f.task.HerdrWorkspaceID, f.task.PaneID, f.task.PaneLabel, "posse-shop-t1-1"); err != nil {
		t.Fatal(err)
	}
	fake.SnapshotValue.Panes[0].Agent = ""
	fake.SnapshotValue.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	fake.Results["pane.process_info"] = json.RawMessage(fmt.Sprintf(`{"process_info":{"pane_id":"w2:p1","shell_pid":%d,"foreground_process_group_id":%d,"foreground_processes":[{"pid":%d,"name":"claude","cwd":%q}]}}`, shell.Process.Pid, worker.Process.Pid, worker.Process.Pid, f.worktree))
	work := filepath.Join(f.worktree, "late-worker-work.txt")
	adapter := &changingSnapshotAdapter{Fake: fake, snapshot: fake.SnapshotValue, autoCloseWhenFile: work, autoClosePaneID: f.task.PaneID}
	f.service.Herdr = adapter
	if _, err := f.service.prepareProject(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("Task after late Worker write: %#v %v", task, err)
	}
	if content := gitTest(t, f.repo, "show", "refs/heads/posse/t1-orphan:late-worker-work.txt"); content != "preserve me\n" {
		t.Fatalf("late work missing from snapshot: %q", content)
	}
	if held, err := f.db.SandboxByTask(ctx, task.ID); err == nil {
		t.Fatalf("Sandbox %d was not released: %#v", sandbox.ID, held)
	}
	_ = work
}

func TestPRWatchDoesNotTeardownWorkingTaskWithMergedPR(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	sandbox := attachPRFixtureSandbox(t, fixture)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(`[defaults]
merge_mode = "pr"
merge_method = "squash"
auto_teardown = "finished"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.SetTaskGatedSHA(ctx, fixture.task.ID, fixture.headSHA); err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, fixture.headSHA)

	if _, err := fixture.service.prepareProject(ctx, fixture.db, fixture.project); err != nil {
		t.Fatalf("reconcile working Task: %v", err)
	}
	task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerged {
		t.Fatalf("merged PR did not Merge working Task: %#v, %v", task, err)
	}
	fake, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok || !containsPane(fake.SnapshotValue, "w2:p1") {
		t.Fatal("working Task Worker pane was closed")
	}
	preservedSandbox, err := fixture.db.SandboxByTask(ctx, task.ID)
	if err != nil || preservedSandbox.ID != sandbox.ID || preservedSandbox.State != "held" {
		t.Fatalf("working Task Sandbox was not preserved: %#v, %v", preservedSandbox, err)
	}
}

func TestPRWatchActiveTaskRequiresVerifiedMergedHeadAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, state, source, branch, base, head string
		verified, merge                         bool
	}{
		{name: "verified working", state: "MERGED", verified: true, merge: true},
		{name: "verified needs decision", state: "MERGED", verified: true, merge: true},
		{name: "open PR", state: "OPEN", verified: true},
		{name: "unverified head", state: "MERGED"},
		{name: "different head", state: "MERGED", verified: true, head: strings.Repeat("f", 40)},
		{name: "wrong source", state: "MERGED", verified: true, source: "other/shop"},
		{name: "wrong branch", state: "MERGED", verified: true, branch: "posse/other"},
		{name: "wrong base", state: "MERGED", verified: true, base: "develop"},
		{name: "wrong recorded URL", state: "MERGED", verified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateWorking)
			ctx := context.Background()
			url := "https://github.com/acme/shop/pull/17"
			if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, url, ""); err != nil {
				t.Fatal(err)
			}
			if tc.name == "verified needs decision" {
				if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateWorking, store.StateNeedsDecision, "worker", "waiting"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.verified {
				verifiedURL := url
				if tc.name == "wrong recorded URL" {
					verifiedURL = "https://github.com/acme/shop/pull/18"
				}
				if err := fixture.db.RecordVerifiedPRHead(ctx, fixture.task.ID, verifiedURL, fixture.headSHA); err != nil {
					t.Fatal(err)
				}
			}
			if tc.source != "" {
				t.Setenv("POSSE_TEST_GH_SOURCE", tc.source)
			}
			if tc.branch != "" {
				t.Setenv("POSSE_TEST_GH_HEAD_BRANCH", tc.branch)
			}
			if tc.base != "" {
				t.Setenv("POSSE_TEST_GH_BASE_BRANCH", tc.base)
			}
			if tc.head != "" {
				fixture.setGHHead(t, tc.head)
			}
			t.Setenv("POSSE_TEST_GH_VIEW_STATE", tc.state)
			fixture.setGraphQLState(t, tc.state, "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, map[bool]string{true: tc.head, false: fixture.headSHA}[tc.head != ""])
			cfg, err := config.Load(fixture.home, fixture.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			task, err := fixture.db.Task(ctx, fixture.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			want := store.StateWorking
			if tc.name == "verified needs decision" {
				want = store.StateNeedsDecision
			}
			if tc.merge || tc.name == "unverified head" || tc.name == "wrong recorded URL" {
				// The observed head is on the Sandbox's Task branch, even if no
				// publish record was retained.
				want = store.StateMerged
			}
			if task.State != want || (want != store.StateMerged && task.MergedRef != "") {
				t.Fatalf("state=%s ref=%s, want=%s", task.State, task.MergedRef, want)
			}
			notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			wantNotices := 0
			if want == store.StateMerged {
				wantNotices = 1
			}
			if got := countNoticeKind(notices, "pr_merged"); got != wantNotices {
				t.Fatalf("merge Notices=%d want=%d", got, wantNotices)
			}
		})
	}
}

func TestPRObservationRejectsStaleTaskIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *prMergeFixture)
	}{
		{"new PR", func(t *testing.T, f *prMergeFixture) {
			t.Helper()
			if err := f.db.UpdateTaskMerge(context.Background(), f.task.ID, "https://github.com/acme/shop/pull/18", ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"new head", func(t *testing.T, f *prMergeFixture) {
			t.Helper()
			if err := f.db.SetTaskGatedSHA(context.Background(), f.task.ID, strings.Repeat("f", 40)); err != nil {
				t.Fatal(err)
			}
		}},
		{"new state", func(t *testing.T, f *prMergeFixture) {
			t.Helper()
			if err := f.db.Transition(context.Background(), f.task.ID, store.StateDone, store.StateWorking, "lead", "follow-up"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPRMergeFixture(t, "pr", store.StateDone)
			ctx := context.Background()
			if err := f.db.UpdateTaskMerge(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetTaskGatedSHA(ctx, f.task.ID, f.headSHA); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.db.Task(ctx, f.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, f)
			observed := store.PRObservation{ProjectID: f.project.ID, TaskID: f.task.ID, PRURL: snapshot.PRURL, HeadSHA: snapshot.GatedSHA, State: "MERGED", Checks: "{}", MergeCommit: f.headSHA}
			effect := prObservationEffect(f.project, snapshot, observed, nil, store.PRObservation{}, false)
			if _, err := f.db.RecordPRObservation(ctx, observed, effect, snapshot); err != nil {
				t.Fatal(err)
			}
			current, err := f.db.Task(ctx, f.project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if current.State == store.StateMerged {
				t.Fatalf("stale observation merged Task: %#v", current)
			}
		})
	}
}

func attachPRFixtureSandbox(t *testing.T, fixture *prMergeFixture) store.Sandbox {
	t.Helper()
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state) VALUES(?,1,?,'idle')`, fixture.project.ID, fixture.worktree); err != nil {
		t.Fatal(err)
	}
	sandbox, err := fixture.db.AcquireSandbox(ctx, fixture.project.ID, fixture.task.ID, filepath.Join(fixture.root, "pool"))
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Path != fixture.worktree || sandbox.State != "held" || sandbox.TaskID != fixture.task.ID {
		t.Fatalf("fixture Sandbox is not attached to the Task worktree: %#v", sandbox)
	}
	return sandbox
}

func containsPane(snapshot herdr.Snapshot, paneID string) bool {
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return true
		}
	}
	return false
}

func TestPRWatchRetainsMergeabilityWhenGitHubReportsUnknown(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	poll := func() {
		t.Helper()
		if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	poll()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "UNKNOWN", "", fixture.headSHA)
	poll()
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	poll()
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "merge_ready") != 1 {
		t.Fatalf("UNKNOWN mergeability re-emitted readiness Notice: notices=%#v err=%v", notices, err)
	}
}

func TestPRWatchClosedPRRaisesDecisionOnce(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "CLOSED", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerging {
		t.Fatalf("closed PR moved Task unexpectedly: %#v, %v", task, err)
	}
	before, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(fixture.ghLog)
	if err != nil || strings.Count(string(after), "api graphql") != strings.Count(string(before), "api graphql")+1 {
		t.Fatalf("closed PR was not watched for reopening: %q %q %v", before, after, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_closed") != 1 || countNoticeKind(notices, "pr_watch_failing") != 0 {
		t.Fatalf("closed Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_closed"), notices, err)
	}
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "pr_closed" || decisions[0].Origin != "pr_closed:"+task.PRURL {
		t.Fatalf("closed PR Decision: %#v %v", decisions, err)
	}
}

func TestPRMergeAutoTeardownRetriesAfterHerdrReturns(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"merged\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", fixture.headSHA, fixture.headSHA)
	availableHerdr, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the fake Herdr adapter")
	}
	fixture.service.Herdr = nil
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerged {
		t.Fatalf("PR merge was not recorded while Herdr was unavailable: %#v, %v", task, err)
	}
	snapshot := availableHerdr.SnapshotValue
	snapshot.Agents = []herdr.Agent{{Name: "posse-shop-t1-1", PaneID: "w2:p1"}}
	adapter := &changingSnapshotAdapter{Fake: availableHerdr, snapshot: snapshot}
	fixture.service.Herdr = adapter
	removeFixtureTaskPane(fixture)
	if err := fixture.service.maintainProjectWatch(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatalf("background watch after Herdr returned: %v; calls=%#v panes=%#v", err, adapter.Calls, adapter.snapshot.Panes)
	}
	task, err = fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("merged Task did not auto-teardown on the next healthy reconcile: %#v, %v", task, err)
	}
}

func TestPRWatchRaisesMergeReadyDecisionAfterObservation(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	if code, output, stderr := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR: %d %s %s", code, output, stderr)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || task.State != store.StateMerging {
		t.Fatalf("fixture Task is not merging: %#v, %v", task, err)
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", task.GatedSHA)
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(`[defaults]
pr_poll = "1ms"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.service.Herdr = nil
	if err := fixture.service.maintainProjectWatch(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatalf("maintain Project Watch: %v", err)
	}
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "merge_ready" {
		notices, _ := fixture.db.Notices(context.Background(), fixture.project.ID, true)
		t.Fatalf("PR Watch did not raise the merge_ready Decision in the polling cycle: decisions=%#v, notices=%#v, err=%v", decisions, notices, err)
	}
}

func TestPRWatchAttributesPartialGraphQLErrorsAndKeepsGlobalFailures(t *testing.T) {
	for _, test := range []struct {
		name, alias string
		global      bool
	}{
		{name: "missing old PR", alias: "pr2"},
		{name: "unattributed error", alias: "unknown", global: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPRMergeFixture(t, "pr", store.StateDone)
			defer fixture.db.Close()
			if code, out, stderr := fixture.run("merge", "t1"); code != 0 {
				t.Fatalf("open PR: %d %s %s", code, out, stderr)
			}
			ctx := context.Background()
			badID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 2, Type: "change", Title: "old PR", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/old", BaseRef: "main"})
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct{ from, to store.State }{
				{store.StateSpawning, store.StateWorking}, {store.StateWorking, store.StateDone}, {store.StateDone, store.StateMerging},
			} {
				source := "cli"
				if step.to == store.StateDone {
					source = "worker"
				}
				if err := fixture.db.Transition(ctx, badID, step.from, step.to, source, "old PR"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET pr_url=? WHERE id=?`, "https://github.com/acme/shop/pull/41", badID); err != nil {
				t.Fatal(err)
			}
			fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
			encoded, err := os.ReadFile(fixture.ghState)
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(encoded, &response); err != nil {
				t.Fatal(err)
			}
			response["data"].(map[string]any)["repo1"].(map[string]any)["pr2"] = nil
			response["errors"] = []any{map[string]any{"message": "PR 41 is inaccessible", "path": []string{"repo1", test.alias}}}
			encoded, err = json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.ghState, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.ghState+".exit", nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(fixture.home, fixture.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := fixture.db.LatestPRObservation(ctx, fixture.task.ID); err != nil {
				t.Fatalf("valid PR starved by error: %v", err)
			}
			state, err := fixture.db.ProjectWatchState(ctx, fixture.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if test.global {
				if state.PRConsecutiveFailures != 3 || countNoticeKind(notices, "pr_watch_failing") == 0 {
					t.Fatalf("unattributed GraphQL failure hidden: state=%#v notices=%#v", state, notices)
				}
			} else {
				if state.PRConsecutiveFailures != 0 {
					t.Fatalf("target-specific error counted as global: %#v", state)
				}
				badFailure := false
				for _, notice := range notices {
					if notice.TaskID == badID && notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "PR 41 is inaccessible") {
						badFailure = true
					}
				}
				if !badFailure {
					t.Fatalf("old PR has no attributed failure: %#v", notices)
				}
			}
		})
	}
}

func TestForgeDetectionFailureRaisesOneProjectNoticeForMultipleTasks(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open first PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	ctx := context.Background()
	secondID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 2, Type: "change", Title: "second PR", State: store.StateSpawning,
		MergeMode: "pr", Branch: "posse/t2", BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{
		{store.StateSpawning, store.StateWorking, "cli"},
		{store.StateWorking, store.StateDone, "worker"},
		{store.StateDone, store.StateMerging, "cli"},
	} {
		if transition.to == store.StateDone {
			if _, err := fixture.db.AddSignal(ctx, secondID, "done", "Worker completed second PR", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.db.Transition(ctx, secondID, transition.from, transition.to, transition.source, "second PR"); err != nil {
			t.Fatal(err)
		}
	}
	secondURL := "https://github.com/acme/shop/pull/18"
	if err := fixture.db.UpdateTaskMerge(ctx, secondID, secondURL, ""); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "config", "remote.origin.url", "https://git.example.com/acme/shop.git")
	gitTest(t, fixture.repo, "config", "url.file://"+fixture.remote+".insteadOf", "https://git.example.com/acme/shop.git")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	state, err := fixture.db.ProjectWatchState(ctx, fixture.project.ID)
	if err != nil || state.PRConsecutiveFailures != 3 {
		t.Fatalf("host-level forge failure streak = %#v, %v", state, err)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var projectFailures, taskFailures int
	for _, notice := range notices {
		if notice.Kind != "pr_watch_failing" {
			continue
		}
		if notice.TaskID == 0 {
			projectFailures++
		} else {
			taskFailures++
		}
	}
	var taskRows int
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pr_task_watch_failures`).Scan(&taskRows); err != nil {
		t.Fatal(err)
	}
	if projectFailures != 1 || taskFailures != 0 || taskRows != 0 {
		t.Fatalf("shared forge detection failure was duplicated per Task: project notices=%d task notices=%d failure rows=%d notices=%#v", projectFailures, taskFailures, taskRows, notices)
	}
}

func TestPRWatchFailureNoticeRequiresThreeConsecutiveFailures(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_FAIL", "1")
	for index := 0; index < 4; index++ {
		if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "pr_watch_failing") != 1 {
		t.Fatalf("watch failure Notice count=%d notices=%#v err=%v", countNoticeKind(notices, "pr_watch_failing"), notices, err)
	}
	state, err := fixture.db.ProjectWatchState(context.Background(), fixture.project.ID)
	if err != nil || state.PRConsecutiveFailures != 4 {
		t.Fatalf("failure streak was not tracked: %#v, %v", state, err)
	}
}

func TestPRStateAppearsInShowAndProjectHome(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	if err := fixture.service.pollProjectPullRequests(context.Background(), fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	code, show, errOutput := fixture.run("show", "t1")
	if code != 0 || !strings.Contains(show, "pr:") || !strings.Contains(show, "https://github.com/acme/shop/pull/17") || !strings.Contains(show, "pending") {
		t.Fatalf("show omitted PR status: exit=%d output=%s error=%s", code, show, errOutput)
	}
	code, home, errOutput := fixture.run("--full")
	if code != 0 || !strings.Contains(home, "pr_state") || !strings.Contains(home, "OPEN") {
		t.Fatalf("Project home omitted Task PR state: exit=%d output=%s error=%s", code, home, errOutput)
	}
}

func TestPRWatchReconcilesOnlyAfterConfiguredInterval(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\npr_poll = \"1h\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	fixture.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	if code, output, errOutput := fixture.run("show", "t1"); code != 0 {
		t.Fatalf("show before the poll interval exit=%d output=%s error=%s", code, output, errOutput)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil || strings.Contains(string(log), "api graphql") {
		t.Fatalf("PR was polled before defaults.pr_poll: %q, %v", log, err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE project_watch_state SET pr_polled_at=? WHERE project_id=?`, time.Now().Add(-2*time.Hour).UnixMilli(), fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.maintainProjectWatch(context.Background(), fixture.db, fixture.project); err != nil {
		t.Fatalf("Watch poll after the configured interval: %v", err)
	}
	log, err = os.ReadFile(fixture.ghLog)
	observation, observationErr := fixture.db.LatestPRObservation(context.Background(), fixture.task.ID)
	if err != nil || strings.Count(string(log), "api graphql") != 1 || observationErr != nil || decodeChecks(observation.Checks).State != "FAILURE" {
		t.Fatalf("stale PR poll was not reconciled: log=%q observation=%#v errors=%v/%v", log, observation, err, observationErr)
	}
}

func TestWatchPollsPRsAtConfiguredInterval(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	configText := "[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\nauto_teardown = \"never\"\npr_poll = \"20ms\"\n"
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	if _, err := fixture.db.AckNotices(context.Background(), fixture.project.ID, []string{"all"}); err != nil {
		t.Fatal(err)
	}
	fake, ok := fixture.service.Herdr.(*herdr.Fake)
	if !ok {
		t.Fatal("fixture does not use the isolated fake Herdr adapter")
	}
	fake.SnapshotValue.Panes[0].AgentStatus = "working"
	fixture.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", fixture.headSHA)
	fixture.test.Chdir(fixture.repo)
	fixture.output.Reset()
	fixture.errOut.Reset()
	herdrCallsBefore := fake.CallCount("session.snapshot")
	cli := fixture.service.CLI()
	cli.Out = &fixture.output
	cli.ErrOut = &fixture.errOut
	done := make(chan int, 1)
	go func() { done <- cli.Run([]string{"watch", "--timeout", "3000"}) }()

	if !waitForPRCondition(done, func() bool {
		log, err := os.ReadFile(fixture.ghLog)
		return err == nil && strings.Count(string(log), "api graphql") >= 1 && fake.CallCount("session.snapshot") > herdrCallsBefore
	}) {
		t.Fatal("watch returned before polling the watched PR")
	}
	if !waitForPRCondition(done, func() bool {
		log, err := os.ReadFile(fixture.ghLog)
		return err == nil && strings.Count(string(log), "api graphql") >= 2
	}) {
		t.Fatal("watch did not poll again at the configured PR interval")
	}
	herdrCallsAfterInitialReconcile := fake.CallCount("session.snapshot")
	if code := <-done; code != 0 || !strings.Contains(fixture.output.String(), "timeout") {
		t.Fatalf("watch did not finish its bounded wait: exit=%d output=%s error=%s", code, fixture.output.String(), fixture.errOut.String())
	}
	if got := fake.CallCount("session.snapshot"); got != herdrCallsAfterInitialReconcile || herdrCallsAfterInitialReconcile <= herdrCallsBefore {
		t.Fatalf("short PR cadence also repeated Herdr reconciliation: snapshots=%d after initial reconcile=%d before=%d", got, herdrCallsAfterInitialReconcile, herdrCallsBefore)
	}
}

func waitForPRCondition(done <-chan int, condition func() bool) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return true
		}
		select {
		case <-done:
			return condition()
		case <-ticker.C:
		}
	}
}

func countNoticeKind(notices []store.Notice, kind string) int {
	count := 0
	for _, notice := range notices {
		if notice.Kind == kind {
			count++
		}
	}
	return count
}

func updateBareMain(t *testing.T, fixture *prMergeFixture, filename, contents string) string {
	t.Helper()
	checkout := filepath.Join(fixture.root, "upstream-change")
	gitTest(t, fixture.root, "clone", "--branch", "main", fixture.remote, checkout)
	gitTest(t, checkout, "config", "user.name", "Posse test")
	gitTest(t, checkout, "config", "user.email", "posse-test@example.test")
	if err := os.WriteFile(filepath.Join(checkout, filename), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, checkout, "add", filename)
	gitTest(t, checkout, "commit", "-m", "upstream change")
	gitTest(t, checkout, "push", "origin", "main")
	return strings.TrimSpace(gitTest(t, checkout, "rev-parse", "HEAD"))
}

func TestProjectSyncFastForwardsCleanDefaultBranch(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	upstream := updateBareMain(t, fixture, "upstream.txt", "origin update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "updated" || strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "main")) != upstream {
		t.Fatalf("clean default branch was not fast-forwarded: %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(filepath.Join(fixture.repo, "upstream.txt")); err != nil || string(contents) != "origin update\n" {
		t.Fatalf("upstream file missing after sync: %q, %v", contents, err)
	}
}

func TestProjectSyncUpdatesDefaultBranchWhenItIsNotCheckedOut(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	gitTest(t, fixture.repo, "checkout", "-b", "lead-work")
	dirty := filepath.Join(fixture.repo, "dirty-lead-file.txt")
	if err := os.WriteFile(dirty, []byte("preserve this dirty file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := updateBareMain(t, fixture, "branch-update.txt", "branch update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "updated" || strings.TrimSpace(gitTest(t, fixture.repo, "branch", "--show-current")) != "lead-work" || strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main")) != upstream {
		t.Fatalf("unchecked-out default branch was not safely updated: %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(dirty); err != nil || string(contents) != "preserve this dirty file\n" {
		t.Fatalf("updating an unchecked-out branch changed the root worktree: %q, %v", contents, err)
	}
}

func TestProjectSyncRefusesWhenDefaultBranchIsCheckedOutElsewhere(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	gitTest(t, fixture.repo, "checkout", "-b", "lead-work")
	otherWorktree := filepath.Join(fixture.root, "main-checkout")
	gitTest(t, fixture.repo, "worktree", "add", otherWorktree, "main")
	upstream := updateBareMain(t, fixture, "other-worktree-update.txt", "remote update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "root_behind" || !strings.Contains(result.Reason, "another worktree") {
		t.Fatalf("sync moved a default branch checked out elsewhere: %#v, %v", result, err)
	}
	if got := strings.TrimSpace(gitTest(t, otherWorktree, "rev-parse", "HEAD")); got == upstream {
		t.Fatalf("the other checked-out default branch unexpectedly advanced to %s", upstream)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 1 {
		t.Fatalf("blocked sync did not create one root_behind Notice: %#v, %v", notices, err)
	}
}

func TestProjectSyncFastForwardsWithUnrelatedUntrackedFiles(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	localPath := filepath.Join(fixture.repo, "AGENTS.md")
	localContents := []byte("preserve this untracked file\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := updateBareMain(t, fixture, "upstream.txt", "origin update\n")
	code, output, errOutput := fixture.run("sync")
	if code != 0 || !strings.Contains(output, "updated") {
		t.Fatalf("posse sync did not fast-forward with unrelated untracked files: exit=%d output=%s error=%s", code, output, errOutput)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD")); got != upstream {
		t.Fatalf("posse sync left the branch at %s, want %s", got, upstream)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("sync changed unrelated untracked bytes: %q, %v", contents, err)
	}
}

func TestProjectSyncWithUntrackedFilesReportsCurrentWhenAlreadyUpToDate(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	localPath := filepath.Join(fixture.repo, "AGENTS.md")
	localContents := []byte("preserve this untracked file\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "current" {
		t.Fatalf("sync with no upstream changes should report current: %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("sync changed unrelated untracked bytes: %q, %v", contents, err)
	}
}

func TestProjectSyncWatcherFastForwardsWithUnrelatedUntrackedFiles(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	localPath := filepath.Join(fixture.repo, "AGENTS.md")
	localContents := []byte("preserve this untracked file\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := updateBareMain(t, fixture, "upstream.txt", "origin update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, false)
	if err != nil || result.Status != "updated" || strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD")) != upstream {
		t.Fatalf("watcher did not fast-forward with unrelated untracked files: %#v, %v", result, err)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("watcher changed unrelated untracked bytes: %q, %v", contents, err)
	}
}

func TestProjectSyncLeavesDirtyRootBytesUntouchedAndDeduplicatesNotice(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	tracked := filepath.Join(fixture.repo, "README.md")
	if err := os.WriteFile(tracked, []byte("local dirty edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(fixture.repo, "untracked-preserve.txt")
	if err := os.WriteFile(untracked, []byte("preserve this untracked file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeTree := snapshotProjectFiles(t, fixture.repo)
	updateBareMain(t, fixture, "dirty-update.txt", "origin update\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		result, syncErr := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
		if syncErr != nil || result.Status != "root_behind" || !strings.Contains(result.Reason, "README.md") {
			t.Fatalf("dirty root should remain behind and name its tracked change: %#v, %v", result, syncErr)
		}
	}
	if afterTree := snapshotProjectFiles(t, fixture.repo); !reflect.DeepEqual(afterTree, beforeTree) {
		t.Fatalf("sync changed Project worktree bytes: before=%#v after=%#v", beforeTree, afterTree)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 1 || !strings.Contains(notices[0].Summary, "README.md") {
		t.Fatalf("root_behind Notice did not name the tracked blocker: notices=%#v err=%v", notices, err)
	}
	if err := os.WriteFile(tracked, []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(untracked); err != nil {
		t.Fatal(err)
	}
	code, output, errOutput := fixture.run("sync")
	if code != 0 || !strings.Contains(output, "updated") {
		t.Fatalf("explicit sync did not retry after the root was cleaned: exit=%d output=%s error=%s", code, output, errOutput)
	}
	if _, err := os.Stat(filepath.Join(fixture.repo, "dirty-update.txt")); err != nil {
		t.Fatalf("explicit sync did not fast-forward the root after cleanup: %v", err)
	}
}

func TestProjectSyncRefusesToOverwriteUntrackedLocalPath(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	localPath := filepath.Join(fixture.repo, "AGENTS.md")
	localContents := []byte("preserve user content\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	before := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD"))
	updateBareMain(t, fixture, "AGENTS.md", "upstream content\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "root_behind" || !strings.Contains(result.Reason, "AGENTS.md") {
		t.Fatalf("sync did not identify the colliding untracked path: result=%#v err=%v", result, err)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD")); got != before {
		t.Fatalf("sync moved default branch across local content: before=%s after=%s", before, got)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("sync changed local untracked bytes: %q err=%v", contents, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 1 || !strings.Contains(notices[0].Summary, "AGENTS.md") {
		t.Fatalf("root_behind Notice did not name the untracked blocker: notices=%#v err=%v", notices, err)
	}
}

func TestProjectSyncRefusesToOverwriteIgnoredLocalPath(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	exclude := filepath.Join(fixture.repo, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte("/generated.cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(fixture.repo, "generated.cache")
	localContents := []byte("user cache must survive\n")
	if err := os.WriteFile(localPath, localContents, 0o600); err != nil {
		t.Fatal(err)
	}
	before := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main"))
	updateBareMain(t, fixture, "generated.cache", "tracked upstream content\n")
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if err != nil || result.Status != "root_behind" || !strings.Contains(result.Reason, "generated.cache") {
		t.Fatalf("sync advanced across an ignored local path: result=%#v err=%v", result, err)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main")); got != before {
		t.Fatalf("sync moved default branch across ignored local content: before=%s after=%s", before, got)
	}
	if contents, err := os.ReadFile(localPath); err != nil || !bytes.Equal(contents, localContents) {
		t.Fatalf("sync changed ignored local bytes: %q err=%v", contents, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 1 {
		t.Fatalf("sync did not report one root_behind Notice: notices=%#v err=%v", notices, err)
	}
}

func snapshotProjectFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[relative] = contents
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestProjectSyncLeavesLocalCommitsAheadOfOriginUntouched(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	if err := os.WriteFile(filepath.Join(fixture.repo, "local-only.txt"), []byte("keep local commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.repo, "add", "local-only.txt")
	gitTest(t, fixture.repo, "commit", "-m", "local only")
	before := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD"))
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	after := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "HEAD"))
	if err != nil || result.Status != "local_ahead" || after != before {
		t.Fatalf("sync changed a local commit ahead of origin: %#v, before=%s after=%s err=%v", result, before, after, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil || countNoticeKind(notices, "root_behind") != 0 {
		t.Fatalf("local-ahead checkout emitted a false root_behind Notice: %#v, %v", notices, err)
	}
}

func TestPRWatchMissingGraphQLRepositoryDataEscalatesAtProjectScope(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	defer fixture.db.Close()
	if code, output, errOutput := fixture.run("merge", "t1"); code != 0 {
		t.Fatalf("open PR exit=%d output=%s error=%s", code, output, errOutput)
	}
	if err := os.WriteFile(fixture.ghState, []byte("{\"data\":{}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for attempt := 0; attempt < 3; attempt++ {
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var failures []store.Notice
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" {
			failures = append(failures, notice)
		}
	}
	if len(failures) != 1 || failures[0].TaskID != 0 {
		t.Fatalf("missing GraphQL repository data created non-Project failures: %#v", failures)
	}
	state, err := fixture.db.ProjectWatchState(ctx, fixture.project.ID)
	if err != nil || state.PRConsecutiveFailures != 3 {
		t.Fatalf("Project failure streak=%d err=%v, want 3", state.PRConsecutiveFailures, err)
	}
}
