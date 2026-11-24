package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

const rewriteQuote = "approve hết nhé"

// newPublishedRewriteFixture publishes a quick-review PR Task, records a pass
// for the published head and delivers a fix instruction bound to that head.
func newPublishedRewriteFixture(t *testing.T) *prMergeFixture {
	t.Helper()
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	installValidationPlanForReview(t, f, "quick")
	if code, output, stderr := f.run("publish", "Worker completion summary"); code != 0 {
		t.Fatalf("first publish: %d %s %s", code, output, stderr)
	}
	repository, err := gitCommonDir(context.Background(), f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	recordCandidateReviewPass(t, f, f.headSHA)
	queueDeliveredValidationFix(t, f)
	currentTask, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.task.Launches = currentTask.Launches
	claim, err := f.db.ClaimBranchAuthor(context.Background(), repository, f.task.Branch, f.task.ID, currentTask.Launches, f.headSHA, f.headSHA, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TASK_GENERATION", fmt.Sprint(claim.Generation))
	return f
}

// amend rewrites the published commit and prepares the fake forge to report
// the rewritten head on the existing PR.
func (f *prMergeFixture) amend(t *testing.T, changeTree bool) string {
	t.Helper()
	if changeTree {
		if err := os.WriteFile(filepath.Join(f.worktree, "change.txt"), []byte("worker change, cleaned\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.worktree, "add", "change.txt")
	}
	gitTest(t, f.worktree, "commit", "--amend", "-m", "feat: clean worker change")
	head := strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	token, err := f.db.EnsurePRBodyMarker(context.Background(), f.task.ID, "", "https://github.com/acme/shop/pull/17")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_BODY", managedPublishBody("previous body", token))
	openPR := fmt.Sprintf(`[{"url":"https://github.com/acme/shop/pull/17","headRefName":"posse/t1","headRefOid":"%s"}]`, head)
	if err := os.WriteFile(f.ghOpenPRs, []byte(openPR), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setGHHead(t, head)
	return head
}

func (f *prMergeFixture) originHead(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(gitTest(t, f.remote, "rev-parse", "refs/heads/posse/t1"))
}

func (f *prMergeFixture) rewriteNotices(t *testing.T) []map[string]string {
	t.Helper()
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var rewrites []map[string]string
	for _, notice := range notices {
		if notice.Kind != "branch_rewritten" {
			continue
		}
		var data map[string]string
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
			t.Fatal(err)
		}
		rewrites = append(rewrites, data)
	}
	return rewrites
}

func TestPublishAfterRebasingUnpublishedBranch(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	briefPath := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "brief.md")
	if err := os.WriteFile(briefPath, []byte("---\ntype: change\ntitle: E2E Brief title\ndone_when: unpublished rebase publishes\nissues: [1185]\n---\nE2E intent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := gitCommonDir(context.Background(), f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET launches=1 WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task.Launches = 1
	if _, err := f.db.ClaimBranchAuthor(context.Background(), repository, f.task.Branch, f.task.ID, 1, f.headSHA, f.headSHA, false); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TASK_GENERATION", "1")
	base := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "main"))
	gitTest(t, f.repo, "checkout", "--detach", base)
	gitTest(t, f.repo, "commit", "--allow-empty", "-m", "upstream change")
	gitTest(t, f.repo, "push", "origin", "HEAD:main")
	gitTest(t, f.worktree, "fetch", "origin")
	gitTest(t, f.worktree, "rebase", "--onto", "origin/main", base)
	f.headSHA = strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	f.setGHHead(t, f.headSHA)
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nmerge_method = \"squash\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if _, err := f.db.ExecContext(context.Background(), `INSERT OR REPLACE INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,1,?)`, f.task.ID, encoded); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := f.run("signal", "done", "Greenhouse-reviewed rebased head", "--head", f.headSHA); code != 0 {
		t.Fatalf("signal reviewed rebase: %d %s %s", code, output, stderr)
	}
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r-rebase", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA, Outcome: "clean", Mode: "gate"}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record Greenhouse pass: %v %v", recorded, err)
	}

	if code, output, stderr := f.run("publish", "Fixes #1185: Worker completion summary"); code != 0 {
		t.Fatalf("publish rebased unpublished branch: %d %s %s", code, output, stderr)
	}
	if got := f.originHead(t); got != f.headSHA {
		t.Fatalf("origin head = %s, want rebased head %s", got, f.headSHA)
	}
	if log, err := os.ReadFile(f.ghLog); err != nil || !strings.Contains(string(log), "Fixes #1185") {
		t.Fatalf("PR body does not fix #1185: %s %v", log, err)
	}
	publishedHead := f.headSHA
	latestMain := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "origin/main"))
	gitTest(t, f.repo, "checkout", "--detach", latestMain)
	gitTest(t, f.repo, "commit", "--allow-empty", "-m", "another upstream change")
	gitTest(t, f.repo, "push", "origin", "HEAD:main")
	gitTest(t, f.worktree, "fetch", "origin")
	gitTest(t, f.worktree, "rebase", "--onto", "origin/main", latestMain)
	f.headSHA = strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	f.setGHHead(t, f.headSHA)
	if code, output, _ := f.run("publish", "Worker completion summary"); code == 0 || !strings.Contains(output, "publish_rewrite_required") {
		t.Fatalf("publish after rebasing published branch: %d %s", code, output)
	}
	if got := f.originHead(t); got != publishedHead {
		t.Fatalf("refused published rebase moved origin to %s, want %s", got, publishedHead)
	}
}

func TestPublishRefusesRewriteWithoutApprovalAndRecordsApprovedRewrite(t *testing.T) {
	f := newPublishedRewriteFixture(t)
	oldHead := f.headSHA
	newHead := f.amend(t, false)

	code, output, _ := f.run("publish", "Worker completion summary")
	if code != 1 || !strings.Contains(output, "publish_rewrite_required") || !strings.Contains(output, "posse publish --rewrite --user-approved") {
		t.Fatalf("plain publish after amend: exit=%d output=%s", code, output)
	}
	if got := f.originHead(t); got != oldHead {
		t.Fatalf("refused publish moved origin to %s", got)
	}
	if code, output, _ := f.run("publish", "--rewrite", "Worker completion summary"); code == 0 || !strings.Contains(output, "--user-approved") {
		t.Fatalf("rewrite without a quote: exit=%d output=%s", code, output)
	}

	if code, output, stderr := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary"); code != 0 {
		t.Fatalf("approved rewrite: %d %s %s", code, output, stderr)
	}
	if got := f.originHead(t); got != newHead {
		t.Fatalf("approved rewrite left origin at %s, want %s", got, newHead)
	}
	repository, err := gitCommonDir(context.Background(), f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := f.db.BranchAuthor(context.Background(), repository, f.task.Branch)
	if err != nil || claim.HeadSHA != newHead {
		t.Fatalf("approved rewrite claim = %#v, %v; want head %s", claim, err, newHead)
	}
	rewrites := f.rewriteNotices(t)
	if len(rewrites) != 1 || rewrites[0]["from_head"] != oldHead || rewrites[0]["to_head"] != newHead || rewrites[0]["user_quote"] != rewriteQuote {
		t.Fatalf("rewrite record = %#v", rewrites)
	}
	if code, output, _ := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary"); code != 1 || !strings.Contains(output, "rewrite_not_needed") {
		t.Fatalf("rewrite of an already published head: exit=%d output=%s", code, output)
	}
}

func TestPublishRewriteRefusesClaimHeadMismatchBeforePushing(t *testing.T) {
	f := newPublishedRewriteFixture(t)
	oldHead := f.originHead(t)
	baseHead := strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", oldHead+"^"))
	repository, err := gitCommonDir(context.Background(), f.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE branch_authorship SET head_sha=? WHERE repository=? AND branch=?`, baseHead, repository, f.task.Branch); err != nil {
		t.Fatal(err)
	}
	f.amend(t, false)

	code, output, _ := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary")
	if code == 0 || !strings.Contains(output, "branch head changed since the previous author claim") {
		t.Fatalf("rewrite with a stale claim head was not refused: exit=%d output=%s", code, output)
	}
	if got := f.originHead(t); got != oldHead {
		t.Fatalf("refused rewrite moved origin to %s, want %s", got, oldHead)
	}
	if rewrites := f.rewriteNotices(t); len(rewrites) != 0 {
		t.Fatalf("refused rewrite recorded notices: %#v", rewrites)
	}
}

func TestPublishRewriteRecordsBeforePushing(t *testing.T) {
	f := newPublishedRewriteFixture(t)
	oldHead := f.originHead(t)
	f.amend(t, false)
	if _, err := f.db.ExecContext(context.Background(), `CREATE TRIGGER reject_branch_rewrite BEFORE INSERT ON notices WHEN NEW.kind='branch_rewritten' BEGIN SELECT RAISE(ABORT, 'simulated record failure'); END`); err != nil {
		t.Fatal(err)
	}

	code, output, _ := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary")
	if code == 0 || !strings.Contains(output, "simulated record failure") {
		t.Fatalf("publish with failed rewrite record: exit=%d output=%s", code, output)
	}
	if got := f.originHead(t); got != oldHead {
		t.Fatalf("record failure pushed rewritten head %s, want origin unchanged at %s", got, oldHead)
	}
}

func TestPublishRewriteKeepsRecordWhenPushFails(t *testing.T) {
	f := newPublishedRewriteFixture(t)
	oldHead := f.originHead(t)
	newHead := f.amend(t, false)
	hook := filepath.Join(f.remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	code, output, _ := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary")
	if code != 1 || !strings.Contains(output, "pr_push_failed") {
		t.Fatalf("publish with rejected push: exit=%d output=%s", code, output)
	}
	if got := f.originHead(t); got != oldHead {
		t.Fatalf("rejected push moved origin to %s, want %s", got, oldHead)
	}
	rewrites := f.rewriteNotices(t)
	if len(rewrites) != 1 || rewrites[0]["from_head"] != oldHead || rewrites[0]["to_head"] != newHead || rewrites[0]["user_quote"] != rewriteQuote {
		t.Fatalf("rewrite record after rejected push = %#v", rewrites)
	}
}

func TestPublishRewriteLeaseRefusesUnknownOriginHead(t *testing.T) {
	f := newPublishedRewriteFixture(t)
	other := filepath.Join(f.root, "other")
	gitTest(t, f.root, "clone", "-q", "--branch", "posse/t1", f.remote, other)
	gitTest(t, other, "-c", "user.name=Other", "-c", "user.email=other@example.test", "commit", "--allow-empty", "-m", "pushed by someone else")
	gitTest(t, other, "push", "-q", "origin", "posse/t1")
	foreign := f.originHead(t)
	f.amend(t, false)

	code, output, _ := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary")
	if code != 1 || !strings.Contains(output, "publish_rewrite_refused") {
		t.Fatalf("rewrite over a foreign push: exit=%d output=%s", code, output)
	}
	if got := f.originHead(t); got != foreign || len(f.rewriteNotices(t)) != 0 {
		t.Fatalf("refused rewrite changed origin to %s or recorded %#v", got, f.rewriteNotices(t))
	}
}

func TestRewrittenUnchangedTreeKeepsReviewAndMergeGatesNewHead(t *testing.T) {
	f := newPublishedRewriteFixture(t)
	oldHead := f.headSHA
	newHead := f.amend(t, false)
	if code, output, stderr := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary"); code != 0 {
		t.Fatalf("approved rewrite: %d %s %s", code, output, stderr)
	}

	code, output, stderr := f.run("signal", "done", "Cleaned history", "--pr", "https://github.com/acme/shop/pull/17")
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if code != 0 || err != nil || countNoticeKind(notices, "head_discontinuity") != 0 {
		t.Fatalf("done after rewrite: exit=%d output=%s stderr=%s notices=%#v err=%v", code, output, stderr, notices, err)
	}

	f.setGraphQLState(t, "OPEN", "PENDING", "REVIEW_REQUIRED", "MERGEABLE", "", newHead)
	if code, output, stderr := f.run("merge", "t1"); code != 0 {
		t.Fatalf("merge after rewrite: %d %s %s", code, output, stderr)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil || task.State != store.StateMerging || task.GatedSHA != newHead {
		t.Fatalf("merge did not gate the rewritten head: task=%#v err=%v", task, err)
	}
	var carried, rounds int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=? AND from_head=? AND to_head=?`, task.ID, oldHead, newHead).Scan(&carried); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_rounds WHERE ship_task_id=?`, task.ID).Scan(&rounds); err != nil {
		t.Fatal(err)
	}
	notices, err = f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if carried != 1 || rounds != 0 || countNoticeKind(notices, "review_auto_start_skipped") != 0 {
		t.Fatalf("unchanged tree did not keep the Review verdict: carries=%d rounds=%d notices=%#v", carried, rounds, notices)
	}
}

func TestRewrittenChangedTreeNeedsFullDiffReviewRound(t *testing.T) {
	f := newPublishedRewriteFixture(t)
	oldHead := f.headSHA
	newHead := f.amend(t, true)
	if code, output, stderr := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary"); code != 0 {
		t.Fatalf("approved rewrite: %d %s %s", code, output, stderr)
	}
	if code, output, stderr := f.run("signal", "done", "Cleaned history", "--pr", "https://github.com/acme/shop/pull/17"); code != 0 {
		t.Fatalf("done after rewrite: %d %s %s", code, output, stderr)
	}
	ctx := context.Background()
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}

	err = requireSelectedReviewAt(ctx, f.db, f.project, config.Config{}, task, newHead)
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "review_required" {
		t.Fatalf("changed tree kept the old Review verdict: %v", err)
	}
	targets, err := changeCandidateTargets(ctx, f.db, f.project, task)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := reviewRoundRepositories(ctx, f.db, f.project, task, targets)
	if err != nil || len(repositories) != 1 || repositories[0].head != newHead {
		t.Fatalf("review repositories = %#v, %v", repositories, err)
	}
	previous := store.ReviewRound{Round: 1, Repository: f.project.Name, HeadSHA: oldHead, Status: "verdict"}
	scope, err := calculateReviewRoundScope(ctx, repositories[0], previous, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Binding.ScopeKind != "full_fallback" || string(scope.DeltaPatch) != string(scope.Cumulative) || !strings.Contains(string(scope.DeltaPatch), "+worker change, cleaned") {
		t.Fatalf("next Review Round is not a full diff: kind=%s delta=%s", scope.Binding.ScopeKind, scope.DeltaPatch)
	}
}
