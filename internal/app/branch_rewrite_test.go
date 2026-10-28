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
	recordCandidateReviewPass(t, f, f.headSHA)
	queueDeliveredValidationFix(t, f)
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
	rewrites := f.rewriteNotices(t)
	if len(rewrites) != 1 || rewrites[0]["from_head"] != oldHead || rewrites[0]["to_head"] != newHead || rewrites[0]["user_quote"] != rewriteQuote {
		t.Fatalf("rewrite record = %#v", rewrites)
	}
	if code, output, _ := f.run("publish", "--rewrite", "--user-approved", rewriteQuote, "Worker completion summary"); code != 1 || !strings.Contains(output, "rewrite_not_needed") {
		t.Fatalf("rewrite of an already published head: exit=%d output=%s", code, output)
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
