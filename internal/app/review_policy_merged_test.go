package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// mergeOnOrigin lands the commits base..head on origin main the way the
// forge merges a pull request: one squash commit, or one rebased commit per
// commit. It returns the merge commit.
func mergeOnOrigin(t *testing.T, fixture *prMergeFixture, base, head string, rebase bool) string {
	t.Helper()
	gitTest(t, fixture.repo, "fetch", "origin", "main", "refs/heads/posse/t1")
	merge := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/remotes/origin/main"))
	commits := []string{head}
	if rebase {
		commits = strings.Fields(gitTest(t, fixture.repo, "rev-list", "--reverse", base+".."+head))
	}
	for _, commit := range commits {
		tree := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", commit+"^{tree}"))
		merge = strings.TrimSpace(gitTest(t, fixture.repo, "commit-tree", tree, "-p", merge, "-m", "merged "+commit))
	}
	gitTest(t, fixture.repo, "push", "origin", merge+":refs/heads/main")
	return merge
}

func candidateTarget(t *testing.T, fixture *prMergeFixture) string {
	t.Helper()
	candidate, err := fixture.db.LatestValidationCandidate(context.Background(), fixture.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return candidate.TargetSHA
}

func pollMergedPR(t *testing.T, fixture *prMergeFixture, head, merge string) []string {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, head)
	if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var summaries []string
	for _, notice := range notices {
		if notice.Kind == "review_policy_violation" {
			summaries = append(summaries, notice.Summary)
		}
	}
	return summaries
}

// t717: a candidate validated against the target tip merges onto that same
// tip. The merge moves the target, but the merged head had the evidence the
// pre-merge gate required, so no violation is reported.
func TestFullPipelineGatedMergeIsNotReportedAsPolicyViolation(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	head := promoteFullPipelineCandidateForTest(t, fixture)
	merge := mergeOnOrigin(t, fixture, candidateTarget(t, fixture), head, false)
	if violations := pollMergedPR(t, fixture, head, merge); len(violations) != 0 {
		t.Fatalf("Posse-validated merge reported as a review policy violation: %q", violations)
	}
}

func TestFullPipelineGatedRebaseMergeIsNotReportedAsPolicyViolation(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	path := filepath.Join(fixture.home, "config.toml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(content), `merge_method = "squash"`, `merge_method = "rebase"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "commit", "--allow-empty", "-m", "second worker commit")
	head := promoteFullPipelineCandidateForTest(t, fixture)
	merge := mergeOnOrigin(t, fixture, candidateTarget(t, fixture), head, true)
	if violations := pollMergedPR(t, fixture, head, merge); len(violations) != 0 {
		t.Fatalf("Posse-validated rebase merge reported as a review policy violation: %q", violations)
	}
}

// A merge onto a target that moved after local validation still lacks the
// selected evidence.
func TestFullPipelineMergeOntoMovedTargetIsPolicyViolation(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	head := promoteFullPipelineCandidateForTest(t, fixture)
	gitTest(t, fixture.repo, "commit", "--allow-empty", "-m", "another PR lands first")
	gitTest(t, fixture.repo, "push", "origin", "main")
	merge := mergeOnOrigin(t, fixture, candidateTarget(t, fixture), head, false)
	violations := pollMergedPR(t, fixture, head, merge)
	if len(violations) != 1 || !strings.Contains(violations[0], "frozen target moved after local validation") {
		t.Fatalf("merge onto a moved target violations=%q, want one frozen-target violation", violations)
	}
}

func pollOpenPRNotices(t *testing.T, fixture *prMergeFixture, head string, passes int) map[string][]string {
	t.Helper()
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", head)
	for pass := 0; pass < passes; pass++ {
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string][]string{}
	for _, notice := range notices {
		kinds[notice.Kind] = append(kinds[notice.Kind], notice.Summary)
	}
	return kinds
}

func markTaskDone(t *testing.T, fixture *prMergeFixture) {
	t.Helper()
	if err := fixture.db.Transition(context.Background(), fixture.task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
}

// #642: an operational failure of the review gate is surfaced with its cause
// instead of silently dropping merge_ready.
func TestMergeReadyReviewGateOperationalErrorIsSurfaced(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	head := promoteFullPipelineCandidateForTest(t, fixture)
	markTaskDone(t, fixture)
	// Only the Task worktree, where the gate reads the target, loses origin.
	gitTest(t, fixture.repo, "config", "extensions.worktreeConfig", "true")
	gitTest(t, fixture.worktree, "config", "--worktree", "remote.origin.uploadpack", "false")
	notices := pollOpenPRNotices(t, fixture, head, 3)
	if len(notices["merge_ready"]) != 0 {
		t.Fatalf("merge_ready raised although the review gate could not run: %q", notices["merge_ready"])
	}
	failing := notices["pr_watch_failing"]
	if len(failing) != 1 || !strings.Contains(failing[0], "merge_ready review gate") || !strings.Contains(failing[0], "ls-remote") {
		t.Fatalf("operational review gate error not surfaced with its cause: %v", notices)
	}
	gitTest(t, fixture.worktree, "config", "--worktree", "--unset", "remote.origin.uploadpack")
	if notices := pollOpenPRNotices(t, fixture, head, 1); len(notices["merge_ready"]) != 1 {
		t.Fatalf("merge_ready not raised once the review gate recovered: %v", notices)
	}
}

// A policy refusal still withholds merge_ready without a failure Notice.
func TestMergeReadyReviewGateRefusalWithholdsNotice(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	installFullPipelinePlan(t, fixture)
	markTaskDone(t, fixture)
	notices := pollOpenPRNotices(t, fixture, fixture.headSHA, 3)
	if len(notices["merge_ready"]) != 0 || len(notices["pr_watch_failing"]) != 0 {
		t.Fatalf("review refusal notices=%v, want neither merge_ready nor pr_watch_failing", notices)
	}
}
