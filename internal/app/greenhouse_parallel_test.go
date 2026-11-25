package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func hasPendingDecision(decisions []store.Decision, id int64) bool {
	for _, decision := range decisions {
		if decision.ID == id {
			return true
		}
	}
	return false
}

func TestPRTargetSyncDoesNotQueueFreshHead(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	target := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "main"))
	sync, err := f.service.queuePRTargetSyncInstruction(context.Background(), f.db, f.task, "", prTargetFreshness{Branch: "main", Tip: target, Root: f.worktree}, f.headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if sync.Queued || sync.Place.Queued() {
		t.Fatalf("target sync = %+v; want no queue entry for a head that contains target", sync)
	}
	places, err := f.db.MergeQueuePlaces(context.Background(), f.project.ID)
	if err != nil || len(places[f.task.ID]) != 0 {
		t.Fatalf("merge queue places = %+v, %v; want no entry", places[f.task.ID], err)
	}
	instructions, err := f.db.PendingTaskInstructions(context.Background(), f.task.ID)
	if err != nil || len(instructions) != 0 {
		t.Fatalf("target refresh instructions = %+v, %v; want none", instructions, err)
	}
}

func TestPRTargetSyncDoesNotQueueFreshTaskBehindAnotherHead(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	otherID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 2, Type: "change", Title: "queue head", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/t2", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET state='done' WHERE id=?`, otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddSignal(ctx, otherID, "done", "ready", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE signals SET at=1 WHERE task_id=?`, otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.PlaceInMergeQueue(ctx, otherID, "", "main", 1); err != nil {
		t.Fatal(err)
	}
	target := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "main"))
	sync, err := f.service.queuePRTargetSyncInstruction(ctx, f.db, f.task, "", prTargetFreshness{Branch: "main", Tip: target, Root: f.worktree}, f.headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if sync.Queued || sync.Place.Queued() {
		t.Fatalf("target sync = %+v; want no queue entry for a head that contains target", sync)
	}
	places, err := f.db.MergeQueuePlaces(ctx, f.project.ID)
	if err != nil || len(places[f.task.ID]) != 0 {
		t.Fatalf("merge queue places = %+v, %v; want no entry", places[f.task.ID], err)
	}
}

func TestPRTargetSyncWithdrawsQueuedRefreshOnceHeadContainsTarget(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	target := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "main"))
	if _, err := f.db.QueueValidationFixInstruction(ctx, f.task.ID, "freshness", f.headSHA, "freshness:stale", "merge origin/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.queuePRTargetSyncInstruction(ctx, f.db, f.task, "", prTargetFreshness{Branch: "main", Tip: target, Root: f.worktree}, f.headSHA); err != nil {
		t.Fatal(err)
	}
	instructions, err := f.db.PendingTaskInstructions(ctx, f.task.ID)
	if err != nil || len(instructions) != 0 {
		t.Fatalf("pending instructions = %+v, %v; want the stale refresh withdrawn", instructions, err)
	}
}

func TestGreenhouseTargetRefreshSkipsHeadThatAlreadyContainsTarget(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	target := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "main"))
	if _, err := queueTargetMergeInstruction(context.Background(), f.db, f.task, "", prTargetFreshness{Branch: "main", Tip: target, Root: f.worktree}, f.headSHA); err != nil {
		t.Fatal(err)
	}
	instructions, err := f.db.PendingTaskInstructions(context.Background(), f.task.ID)
	if err != nil || len(instructions) != 0 {
		t.Fatalf("target refresh instructions = %+v, %v; want none for a head that contains target %s", instructions, err, target)
	}
}

func TestGreenhouseFindingOnUnchangedAcceptedLineDoesNotBlock(t *testing.T) {
	repo := t.TempDir()
	gitRun := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	gitRun("init", "-q")
	gitRun("config", "user.name", "Test")
	gitRun("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("first\naccepted line\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-qm", "accepted")
	accepted := gitRun("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("target line\nfirst\naccepted line\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-qm", "target merge")
	current := gitRun("rev-parse", "HEAD")
	changed, err := greenhouseFindingChangedSinceAccepted(context.Background(), repo, accepted, current, "main.go", 3, 3)
	if err != nil || changed {
		t.Fatalf("unchanged accepted line changed=%t err=%v; want unchanged", changed, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("target line\nfirst\naccepted line\nchanged last\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-qm", "change line inside finding range")
	current = gitRun("rev-parse", "HEAD")
	changed, err = greenhouseFindingChangedSinceAccepted(context.Background(), repo, accepted, current, "main.go", 2, 4)
	if err != nil || !changed {
		t.Fatalf("multi-line finding changed=%t err=%v; want changed when an end line changed", changed, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("target line\nfirst\nchanged line\nlast\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-qm", "change accepted line")
	current = gitRun("rev-parse", "HEAD")
	changed, err = greenhouseFindingChangedSinceAccepted(context.Background(), repo, accepted, current, "main.go", 3, 3)
	if err != nil || !changed {
		t.Fatalf("changed accepted line changed=%t err=%v; want changed", changed, err)
	}
}

func TestAcceptedGreenhouseHeadUsesTheMatchingRepositoryHistory(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	accepted := f.headSHA
	decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "review_cap", Origin: reviewCapOrigin(f.task, accepted), Question: "Accept this exact head?", Options: []string{"accept-head"}})
	if err != nil {
		t.Fatal(err)
	}
	decision, err = f.db.AnswerDecision(ctx, f.project.ID, decision.ID, "accept-head", "Accept this exact head.")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordReviewDisposition(ctx, f.task.ID, decision.ID, []string{accepted}, decision.UserQuote, "P1", "user", ""); err != nil {
		t.Fatal(err)
	}
	foreignHead := strings.Repeat("b", 40)
	foreignDecision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "review_cap", Origin: reviewCapOrigin(f.task, foreignHead), Question: "Accept another exact head?", Options: []string{"accept-head"}})
	if err != nil {
		t.Fatal(err)
	}
	foreignDecision, err = f.db.AnswerDecision(ctx, f.project.ID, foreignDecision.ID, "accept-head", "Accept this other repository head.")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordReviewDisposition(ctx, f.task.ID, foreignDecision.ID, []string{foreignHead}, foreignDecision.UserQuote, "P1", "user", ""); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "cat-file", "-e", accepted+"^{commit}")
	gitTest(t, f.repo, "merge-base", "--is-ancestor", accepted, accepted)
	got, err := acceptedGreenhouseHead(ctx, f.db, f.task.ID, f.repo, accepted)
	if err != nil || got != accepted {
		t.Fatalf("accepted head for %s=%q err=%v; want repository head %s", f.project.Name, got, err, accepted)
	}
}

func TestAcceptedFindingFollowupSupportsGitLab(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	gitTest(t, f.repo, "config", "remote.origin.url", "https://gitlab.com/acme/shop.git")
	fakeBin := filepath.Join(f.root, "gitlab-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	apiLog := filepath.Join(f.root, "glab.log")
	fakeGlab := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> \"%s\"\ncase \"$*\" in\n  *'/issues?state=opened'*) printf '[]\\n' ;;\n  *'/issues') printf '{\\\"iid\\\":99}\\n' ;;\n  *) exit 1 ;;\nesac\n", apiLog)
	if err := os.WriteFile(filepath.Join(fakeBin, "glab"), []byte(fakeGlab), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	findings := []store.ProviderFinding{{Key: "G-2", Severity: "P1", Path: "worker.go", Line: 17, Excerpt: "accepted finding", Guidance: "fix later"}}
	err := createAcceptedFindingFollowup(context.Background(), greenhouseMember{name: "shop", root: f.repo}, f.task, f.headSHA, findings, config.Config{})
	if err != nil {
		t.Fatalf("file GitLab follow-up issue: %v", err)
	}
	calls, err := os.ReadFile(apiLog)
	if err != nil || !strings.Contains(string(calls), "--method POST") || !strings.Contains(string(calls), "/issues") || !strings.Contains(string(calls), "worker.go:17") {
		t.Fatalf("GitLab issue API calls=%q err=%v", calls, err)
	}
}

func TestGreenhouseTargetMergeAfterAcceptHeadDoesNotRaiseReviewCap(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
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
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(f.worktree, "change.txt"), []byte("worker line 1\nworker line 2\nworker line 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.worktree, "add", "change.txt")
	gitTest(t, f.worktree, "commit", "-m", "prepare multi-line accepted finding")
	accepted := strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "review_cap", Origin: reviewCapOrigin(f.task, accepted), Question: "Accept this exact head?", Options: []string{"accept-head"}})
	if err != nil {
		t.Fatal(err)
	}
	decision, err = f.db.AnswerDecision(ctx, f.project.ID, decision.ID, "accept-head", "Accept this exact head.")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordReviewDisposition(ctx, f.task.ID, decision.ID, []string{accepted}, decision.UserQuote, "P1", "user", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "target.txt"), []byte("target merge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.worktree, "add", "target.txt")
	gitTest(t, f.worktree, "commit", "-m", "merge target")
	current := strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	fakeBin := filepath.Join(f.root, "issue-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	issueLog := filepath.Join(f.root, "issue.log")
	fakeGH := fmt.Sprintf("#!/bin/sh\ncase \"$1 $2\" in\n  'issue list') printf '[]\\n' ;;\n  'issue create') printf '%%s\\n' \"$*\" > \"%s\"; printf 'https://github.com/acme/shop/issues/99\\n' ;;\n  *) exit 1 ;;\nesac\n", issueLog)
	if err := os.WriteFile(filepath.Join(fakeBin, "gh"), []byte(fakeGH), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	result := greenhouse.RoundResult{Grant: "shop/t1/shop/r1", Result: &greenhouse.HostResult{
		Head: current, Target: accepted, Verdict: "findings",
		Findings: []greenhouse.Finding{{ID: "G-1", Priority: "P1", File: "change.txt", Start: 1, End: 3, Explanation: "Previously accepted issue", Action: "Track follow-up", Head: current}},
	}}
	member := greenhouseMember{name: f.project.Name, root: f.repo, worktree: f.worktree}
	recorded, verdict, err := recordGreenhouseVerdict(ctx, f.db, f.task, member, f.root, "P1", config.Config{}, result)
	if err != nil || !recorded || verdict.Blocking != 0 {
		t.Fatalf("record accepted-code findings Verdict: recorded=%t blocking=%d err=%v", recorded, verdict.Blocking, err)
	}
	var reviewCapDecisions int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions WHERE task_id=? AND kind='review_cap'`, f.task.ID).Scan(&reviewCapDecisions); err != nil || reviewCapDecisions != 1 {
		t.Fatalf("review_cap Decisions=%d err=%v; want only the accepted Decision", reviewCapDecisions, err)
	}
	passed, err := f.db.GreenhouseGatePass(ctx, f.task.ID, f.project.Name, current, "P1")
	if err != nil || !passed {
		t.Fatalf("Greenhouse gate pass=%t err=%v; want pass", passed, err)
	}
	issue, err := os.ReadFile(issueLog)
	if err != nil || !strings.Contains(string(issue), "Follow up Greenhouse findings") || !strings.Contains(string(issue), "change.txt:1") {
		t.Fatalf("follow-up issue command=%q err=%v", issue, err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "change.txt"), []byte("worker line 1\nworker line 2\nchanged line 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.worktree, "add", "change.txt")
	gitTest(t, f.worktree, "commit", "-m", "change accepted line")
	changedHead := strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	result.Grant = "shop/t1/shop/r2"
	result.Result.Head = changedHead
	result.Result.Findings[0].Head = changedHead
	recorded, verdict, err = recordGreenhouseVerdict(ctx, f.db, f.task, member, f.root, "P1", config.Config{}, result)
	if err != nil || !recorded || verdict.Blocking != 1 {
		t.Fatalf("record changed-code findings Verdict: recorded=%t blocking=%d err=%v; want blocking", recorded, verdict.Blocking, err)
	}
}

func TestGreenhouseStaleHeadInstructionRequestsReviewWithoutPR(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
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
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queueTargetMergeInstruction(context.Background(), f.db, f.task, "", prTargetFreshness{Branch: "main", Tip: strings.Repeat("a", 40)}, f.headSHA); err != nil {
		t.Fatal(err)
	}
	instructions, err := f.db.PendingTaskInstructions(context.Background(), f.task.ID)
	if err != nil || len(instructions) != 1 || !strings.Contains(instructions[0].Body, "posse signal done \"<summary>\" --head <sha>") || !strings.Contains(instructions[0].Body, "without --pr") || !strings.Contains(instructions[0].Body, "posse publish") {
		t.Fatalf("stale Greenhouse instruction = %+v, %v", instructions, err)
	}
}

func TestGreenhouseCleanVerdictAfterTargetRefreshQueuesPublicationWithinOneReconcile(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
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
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := f.advanceTarget(t, false, true)
	oldVerdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r-old", Head: f.headSHA, Target: strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "HEAD")), ReviewBase: f.headSHA, Outcome: "clean", Mode: "gate"}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), oldVerdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record earlier Verdict: recorded=%v err=%v", recorded, err)
	}
	if code, output, stderr := f.run("signal", "working", "Refreshing the target"); code != 0 {
		t.Fatalf("working before target refresh: code=%d output=%s stderr=%s", code, output, stderr)
	}
	gitTest(t, f.task.WorktreePath, "fetch", "origin", "main")
	gitTest(t, f.task.WorktreePath, "merge", "--no-edit", "origin/main")
	if _, err := f.db.ExecContext(context.Background(), `UPDATE messages SET status='delivered' WHERE task_id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	newHead := strings.TrimSpace(gitTest(t, f.task.WorktreePath, "rev-parse", "HEAD"))
	turn, err := f.db.TaskTurnToken(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := f.run("signal", "done", "Reviewed refreshed head", "--head", newHead, "--turn", turn); code != 0 {
		t.Fatalf("done on refreshed head: code=%d output=%s stderr=%s", code, output, stderr)
	}
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r1", Head: newHead, Target: refreshed, ReviewBase: refreshed, Outcome: "clean", Mode: "gate"}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return newHead, nil }); err != nil || !recorded {
		t.Fatalf("record refreshed-head pass: recorded=%v err=%v", recorded, err)
	}
	if _, err := f.service.queuePRTargetSyncInstruction(context.Background(), f.db, f.task, "", prTargetFreshness{Branch: "main", Tip: refreshed, Root: f.task.WorktreePath}, f.headSHA); err != nil {
		t.Fatalf("check superseded Verdict head against current Task head: %v", err)
	}
	if err := f.service.maintainProjectWatch(context.Background(), f.db, f.project); err != nil {
		t.Fatal(err)
	}
	messages, err := f.db.TaskMessagesAfter(context.Background(), f.task.ID, 0, 10)
	publishInstructions, refreshInstructions := 0, 0
	for _, message := range messages {
		if message.Status == "withdrawn" {
			continue
		}
		if strings.Contains(message.Body, "Greenhouse passed Task head") && strings.Contains(message.Body, "posse publish") {
			publishInstructions++
		}
		if strings.Contains(message.Body, "merge origin/main") {
			refreshInstructions++
		}
	}
	if err != nil || publishInstructions != 1 || refreshInstructions != 0 {
		t.Fatalf("refreshed-head messages=%+v err=%v; want one publication and no superseded target refresh", messages, err)
	}
}

func TestGreenhouseCompleteGreenhouseReviewSupersedesPartialNotice(t *testing.T) {
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
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "done", "Review requested", map[string]string{"greenhouse_pending_head": f.headSHA}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateDone, "worker", "Review requested"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	partialVerdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA, Outcome: "inconclusive", Mode: "gate"}
	if recorded, err := f.db.RecordGreenhouseVerdict(ctx, partialVerdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record partial Verdict: recorded=%t err=%v", recorded, err)
	}
	at := time.Now().UnixMilli()
	if err := f.db.RecordGreenhouseReviewRequest(ctx, store.GreenhouseReviewRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: f.headSHA, Provider: store.GreenhouseReviewProvider, RequestedAt: at, SettledAt: at + 1, Outcome: "verdict", Grant: partialVerdict.Grant}); err != nil {
		t.Fatalf("record partial request: %v", err)
	}
	partial := greenhouse.HostResult{Status: "partial-review", Verdict: "partial", Uncovered: []greenhouse.UncoveredPath{{Path: "large.sql", Reason: "hunk windows exceed 65536 bytes"}}}
	if _, err := f.db.CreateGreenhousePartialNoticeOnce(ctx, f.project.ID, f.task.ID, f.project.Name, f.headSHA, "Partial review", partial); err != nil {
		t.Fatal(err)
	}
	if err := f.service.raiseNoticeDecisions(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(pending) != 1 || pending[0].Kind != "greenhouse_coverage" {
		t.Fatalf("partial coverage Decision = %+v, %v; want one pending", pending, err)
	}
	complete := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r2", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA, Outcome: "findings", Mode: "gate"}
	if recorded, err := f.db.RecordGreenhouseVerdict(ctx, complete, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record complete Verdict: recorded=%t err=%v", recorded, err)
	}
	at = time.Now().UnixMilli()
	if err := f.db.RecordGreenhouseReviewRequest(ctx, store.GreenhouseReviewRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: f.headSHA, Provider: store.GreenhouseReviewProvider, RequestedAt: at, SettledAt: at + 1, Outcome: "verdict", Grant: complete.Grant}); err != nil {
		t.Fatalf("record complete request: %v", err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseRepositoryPrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err := f.db.UndeliveredTaskMessages(ctx, f.task.ID)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "Greenhouse passed Task head") {
		t.Fatalf("publication after complete same-head review = %+v, %v; old partial notice must not require coverage acceptance", messages, err)
	}
	pending, err = f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stale coverage Decisions after complete review = %+v, %v; want none", pending, err)
	}
}

func TestGreenhousePassQueuesPublicationForNeedsDecisionWorker(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
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
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "done", "Review requested", map[string]string{"greenhouse_pending_head": f.headSHA}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateDone, "worker", "Review requested"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateDone, store.StateWorking, "worker", "Publish was refused"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "needs-decision", "Waiting for direction", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateNeedsDecision, "worker", "Waiting for direction"); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `INSERT INTO verdicts(task_id,repo,provider,protocol,head_sha,target_sha,review_base_sha,verdict,blocking,mode,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, f.task.ID, f.project.Name, store.BuiltinReviewProvider, "builtin-v1", f.headSHA, f.headSHA, f.headSHA, "clean", 0, "gate", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseRepositoryPrepublication(context.Background(), f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err := f.db.UndeliveredTaskMessages(context.Background(), f.task.ID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("builtin Verdict queued publication: %+v, %v", messages, err)
	}
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA, Outcome: "clean", Mode: "gate"}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record Gate pass: recorded=%v err=%v", recorded, err)
	}
	cfg, err = config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseRepositoryPrepublication(context.Background(), f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err = f.db.UndeliveredTaskMessages(context.Background(), f.task.ID)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "Greenhouse passed Task head") || !strings.Contains(messages[0].Body, "posse publish") {
		t.Fatalf("publication message = %+v, %v", messages, err)
	}
}

func TestGreenhouseFindingsWithoutPriorReadinessDoNotWithdrawReadiness(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	defer f.db.Close()
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
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	verdict := store.Verdict{
		TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2",
		Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA,
		Outcome: "findings", Blocking: 1, FindingsPath: "findings.md", Mode: "gate",
		Findings: []store.ProviderFinding{{Key: "blocking", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "bad", Guidance: "fix", Head: f.headSHA}},
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record findings Verdict: recorded=%v err=%v", recorded, err)
	}
	oldReady, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "merge_ready", Summary: "old head was ready", DataJSON: marshalJSON(map[string]string{"head_sha": f.headSHA})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE notices SET created_at=1 WHERE id=?`, oldReady); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "done", "Task reopened and completed", map[string]string{"greenhouse_pending_head": f.headSHA}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.handleGreenhouseBlockingVerdict(context.Background(), f.db, f.project, f.task, verdict); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "merge_readiness_withdrawn") != 0 {
		t.Fatalf("merge readiness withdrawal notices = %+v; want none for first-round findings", notices)
	}
	if countNoticeKind(notices, "greenhouse_review_failed") != 1 {
		t.Fatalf("Greenhouse failure notices = %+v; want one findings notice", notices)
	}
}

func TestGreenhouseFindingsWithoutCurrentDoneSignalPreserveMergeReadiness(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	defer f.db.Close()
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
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := f.db.RaiseDecision(context.Background(), store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Origin: "test:merge-ready-no-current-done", Kind: "merge_ready", Question: "Merge Task?", Options: []string{"merge", "wait"}})
	if err != nil {
		t.Fatal(err)
	}
	oldReady, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "merge_ready", Summary: "earlier cycle was ready", DataJSON: marshalJSON(map[string]string{"head_sha": f.headSHA})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE notices SET created_at=1 WHERE id=?`, oldReady); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "working", "Task resumed without a new completion", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA, Outcome: "findings", Blocking: 1, FindingsPath: "findings.md", Mode: "gate", Findings: []store.ProviderFinding{{Key: "blocking", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "bad", Guidance: "fix", Head: f.headSHA}}}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record findings Verdict: recorded=%v err=%v", recorded, err)
	}
	if err := f.service.handleGreenhouseBlockingVerdict(context.Background(), f.db, f.project, f.task, verdict); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "merge_readiness_withdrawn") != 0 {
		t.Fatalf("merge readiness withdrawal notices = %+v; want none without a current done Signal", notices)
	}
	pending, err := f.db.PendingDecisions(context.Background(), f.project.ID)
	if err != nil || !hasPendingDecision(pending, decision.ID) {
		t.Fatalf("merge-ready Decision after Verdict = %+v, %v; want it to remain pending", pending, err)
	}
}

func TestGreenhouseCoverageAcceptanceDoesNotPublishBlockingFindings(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
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
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "done", "review requested", map[string]string{"greenhouse_pending_head": f.headSHA}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateDone, "worker", "review requested"); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	member := greenhouseMember{name: f.project.Name, root: f.task.WorktreePath, worktree: f.task.WorktreePath}
	roundResult := greenhouse.RoundResult{Grant: "shop/t1/shop/r1", Result: &greenhouse.HostResult{Head: f.headSHA, Target: f.headSHA, Verdict: "findings", Uncovered: []greenhouse.UncoveredPath{{Path: "large.sql", Reason: "hunk windows exceed 65536 bytes"}}, Findings: []greenhouse.Finding{{ID: "blocking", Priority: "P1", Origin: "correctness", File: "main.go", Start: 1, End: 1, Explanation: "blocking issue", Action: "fix it", Head: f.headSHA}}}}
	recorded, verdict, err := recordGreenhouseVerdict(ctx, f.db, f.task, member, t.TempDir(), "P1", config.Config{}, roundResult)
	if err != nil || !recorded || verdict.Outcome != "findings" || verdict.Blocking != 1 {
		t.Fatalf("record partial findings Verdict: recorded=%v verdict=%+v err=%v", recorded, verdict, err)
	}
	if err := f.service.handleGreenhouseBlockingVerdict(ctx, f.db, f.project, f.task, verdict); err != nil {
		t.Fatal(err)
	}
	result := roundResult.Result
	if _, err := f.db.CreateGreenhousePartialNoticeOnce(ctx, f.project.ID, f.task.ID, f.project.Name, f.headSHA, "Partial review", result); err != nil {
		t.Fatal(err)
	}
	if err := f.service.raiseNoticeDecisions(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("coverage Decisions = %#v, %v", decisions, err)
	}
	decision, err := f.db.AnswerDecision(ctx, f.project.ID, decisions[0].ID, "accept-coverage-at-exact-head", "Accept coverage of reviewed paths")
	if err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := f.run("apply", fmt.Sprint(decision.ID)); code != 0 {
		t.Fatalf("apply coverage Decision: %s %s", output, stderr)
	}
	accepted, err := greenhouseAcceptHeadDisposition(ctx, f.db, f.task, f.project.Name, f.headSHA)
	if err != nil || accepted {
		t.Fatalf("coverage disposition with blocking findings = %t, %v; want false", accepted, err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseRepositoryPrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	messages, err := f.db.UndeliveredTaskMessages(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if strings.Contains(message.Body, "Publish this exact head") {
			t.Fatalf("blocking partial review queued publication: %+v", messages)
		}
	}
	pending, err := f.db.ReviewFixInstructionPending(ctx, f.task.ID)
	if err != nil || !pending {
		t.Fatalf("blocking findings fix pending = %v, %v; want queued fix", pending, err)
	}
}

func TestReplayedGreenhouseBlockingVerdictWithdrawsReadinessAndQueuesFix(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
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
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	gitTest(t, f.worktree, "push", "origin", "posse/t1:refs/heads/posse/t1")
	if err := os.WriteFile(filepath.Join(f.repo, "target.txt"), []byte("target change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.repo, "add", "target.txt")
	gitTest(t, f.repo, "commit", "-m", "target advance")
	gitTest(t, f.repo, "push", "origin", "main")
	gitTest(t, f.worktree, "fetch", "origin", "main")
	gitTest(t, f.worktree, "merge", "--no-edit", "origin/main")
	gitTest(t, f.worktree, "push", "origin", "posse/t1:refs/heads/posse/t1")
	f.headSHA = strings.TrimSpace(gitTest(t, f.worktree, "rev-parse", "HEAD"))
	f.task, err = f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET gated_sha=? WHERE id=?`, f.headSHA, f.task.ID); err != nil {
		t.Fatal(err)
	}
	target := strings.TrimSpace(gitTest(t, f.repo, "rev-parse", "main"))
	if _, err := queueTargetMergeInstruction(ctx, f.db, f.task, "", prTargetFreshness{Branch: "main", Tip: target, Root: f.worktree}, f.headSHA); err != nil {
		t.Fatal(err)
	}
	instructions, err := f.db.PendingTaskInstructions(ctx, f.task.ID)
	if err != nil || len(instructions) != 0 {
		t.Fatalf("target refresh instructions after target merge = %+v, %v; want none", instructions, err)
	}
	if _, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "merge_ready", Summary: "target-merged head is ready", DataJSON: marshalJSON(map[string]string{"head_sha": f.headSHA})}); err != nil {
		t.Fatal(err)
	}
	verdict := store.Verdict{
		TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2",
		Grant: "shop/t1/shop/r4", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA,
		Outcome: "findings", Blocking: 1, FindingsPath: "findings.json", Mode: "gate",
		Findings: []store.ProviderFinding{{Key: "P-1", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "bad", Guidance: "fix", Head: f.headSHA}},
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record first Verdict: recorded=%t err=%v", recorded, err)
	}
	replayed, err := f.db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return f.headSHA, nil })
	if err != nil || replayed {
		t.Fatalf("replay Verdict: recorded=%t err=%v; want persisted Verdict", replayed, err)
	}
	if err := f.service.applyGreenhouseVerdict(ctx, f.db, f.project, f.task, verdict); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.ReviewFixInstructionPending(ctx, f.task.ID)
	if err != nil || !pending {
		t.Fatalf("replayed blocking Verdict fix instruction pending=%t err=%v", pending, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || countNoticeKind(notices, "greenhouse_review_failed") != 1 || countNoticeKind(notices, "merge_readiness_withdrawn") != 1 {
		t.Fatalf("replayed blocking Verdict notices=%+v err=%v", notices, err)
	}
	providerFindings, err := f.db.ProviderFindingsForTask(ctx, f.task.ID)
	if err != nil || len(providerFindings) != 1 || providerFindings[0].Severity != "P1" {
		t.Fatalf("replayed provider findings=%+v err=%v", providerFindings, err)
	}
	code, output, stderr := f.run("show", "t1", "--format", "json")
	if code != 0 || !strings.Contains(output, `"latest_verdict":"findings"`) || !strings.Contains(output, `"grant":"shop/t1/shop/r4"`) {
		t.Fatalf("posse show did not expose the replayed Verdict: code=%d output=%s stderr=%s", code, output, stderr)
	}
}

func TestGreenhouseBlockingVerdictWithdrawsMergeReadinessAndQueuesFix(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
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
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := f.db.RaiseDecision(context.Background(), store.DecisionRequest{
		ProjectID: f.project.ID, TaskID: f.task.ID, Origin: "test:merge-ready", Kind: "merge_ready",
		Question: "Merge Task?", Options: []string{"merge", "wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := f.db.PendingDecisions(context.Background(), f.project.ID)
	if err != nil || !hasPendingDecision(pendingBefore, decision.ID) {
		t.Fatalf("merge-ready Decision before Verdict = %+v, %v", pendingBefore, err)
	}
	oldWithdrawal, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "merge_readiness_withdrawn", Summary: "withdrawal for an earlier readiness cycle", DataJSON: marshalJSON(map[string]string{"head_sha": f.headSHA, "repository": f.project.Name})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE notices SET created_at=1 WHERE id=?`, oldWithdrawal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "done", "Task reopened and completed", map[string]string{"greenhouse_pending_head": f.headSHA}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "merge_ready", Summary: "current head is ready", DataJSON: marshalJSON(map[string]string{"head_sha": f.headSHA})}); err != nil {
		t.Fatal(err)
	}
	verdict := store.Verdict{
		TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2",
		Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA,
		Outcome: "findings", Blocking: 1, FindingsPath: "findings.md", Mode: "gate",
		Findings: []store.ProviderFinding{{Key: "blocking", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "bad", Guidance: "fix", Head: f.headSHA}},
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record findings Verdict: recorded=%v err=%v", recorded, err)
	}
	if err := f.service.handleGreenhouseBlockingVerdict(context.Background(), f.db, f.project, f.task, verdict); err != nil {
		t.Fatal(err)
	}
	current, err := f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil || current.State != store.StateDone {
		t.Fatalf("Task state = %s, %v; want merge readiness withdrawn to done", current.State, err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	signals, err := f.db.TaskSignals(context.Background(), f.task.ID, 1)
	if err != nil || len(signals) == 0 {
		t.Fatalf("latest Task Signal = %+v, %v", signals, err)
	}
	withdrawn, namedFindings := 0, false
	for _, notice := range notices {
		if notice.Kind != "merge_readiness_withdrawn" {
			continue
		}
		var data struct {
			Head string `json:"head_sha"`
		}
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
			t.Fatal(err)
		}
		if data.Head == verdict.Head && notice.CreatedAt > signals[0].At {
			withdrawn++
			namedFindings = namedFindings || strings.Contains(notice.Summary, "findings.md")
		}
	}
	if withdrawn != 1 || !namedFindings {
		t.Fatalf("notices = %+v, want one current-head withdrawal and findings file", notices)
	}
	pendingAfter, err := f.db.PendingDecisions(context.Background(), f.project.ID)
	if err != nil || hasPendingDecision(pendingAfter, decision.ID) {
		t.Fatalf("stale merge-ready Decision remains pending: %+v, %v", pendingAfter, err)
	}
	pending, err := f.db.ReviewFixInstructionPending(context.Background(), f.task.ID)
	if err != nil || !pending {
		t.Fatalf("Review fix pending = %v, %v; want queued fix", pending, err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSelectedReviewAt(context.Background(), f.db, f.project, cfg, current, f.headSHA); err == nil || !strings.Contains(err.Error(), "no current passing Greenhouse Verdict") {
		t.Fatalf("merge review gate error = %v, want missing passing Verdict for this head", err)
	}
	code, output, _ := f.run("merge", "t1", "--execute", "--user-approved", "approved")
	if code == 0 || !strings.Contains(output, "no current passing Greenhouse Verdict") {
		t.Fatalf("merge --execute = %d %s; want refusal until a passing Verdict exists", code, output)
	}
}

func TestGreenhouseBlockingVerdictWithDisposedFindingsDoesNotFail(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	defer f.db.Close()
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
	if _, err := f.db.ExecContext(context.Background(), `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.task, err = f.db.TaskByID(context.Background(), f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA, Outcome: "findings", Blocking: 1, FindingsPath: "findings.md", Mode: "gate", Findings: []store.ProviderFinding{{Key: "G-1", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "bad", Guidance: "fix", Head: f.headSHA}}}
	if recorded, err := f.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record findings Verdict: recorded=%v err=%v", recorded, err)
	}
	if _, err := f.db.ExecContext(context.Background(), `UPDATE provider_findings SET state='fixed' WHERE task_id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.handleGreenhouseBlockingVerdict(context.Background(), f.db, f.project, f.task, verdict); err != nil {
		t.Fatalf("handle Verdict after finding disposition: %v", err)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "pr_watch_failing") != 0 {
		t.Fatalf("watch failure notices = %+v; want none", notices)
	}
}

func TestGreenhouseReviewDoesNotStartDuringOrAfterTeardown(t *testing.T) {
	for _, tornDown := range []bool{false, true} {
		t.Run(map[bool]string{false: "teardown in progress", true: "already torn down"}[tornDown], func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateDone)
			if tornDown {
				if err := f.db.TransitionWithApproval(context.Background(), f.task.ID, store.StateDone, store.StateTornDown, "user", "discarded", "discard", "approved"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.db.StartIntent(context.Background(), f.project.ID, f.task.ID, "teardown", "started", `{}`, os.Getpid()); err != nil {
					t.Fatal(err)
				}
			}
			roundDir := filepath.Join(f.home, "projects", f.project.Name, "tasks", taskIDString(f.task.Seq), "greenhouse")
			if err := f.service.startParallelGreenhouseReviews(context.Background(), f.db, f.home, f.project, f.task, []store.ReviewTarget{{Repository: f.project.Name, Commit: f.headSHA}}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(parallelReviewOwnerPath(roundDir, f.project.Name)); !os.IsNotExist(err) {
				t.Fatalf("review owner started during teardown: %v", err)
			}
		})
	}
}

func TestGreenhousePrepublicationNoticesSkippedPendingIntent(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		provider   string
		verb       string
		data       any
		wantReason string
	}{
		{name: "missing done signal", provider: store.GreenhouseReviewProvider, wantReason: "no latest done Signal"},
		{name: "latest signal not done", provider: store.GreenhouseReviewProvider, verb: "working", data: map[string]string{}, wantReason: "no latest done Signal"},
		{name: "invalid signal payload", provider: store.GreenhouseReviewProvider, verb: "done", data: "invalid", wantReason: "invalid or missing greenhouse_pending_head"},
		{name: "empty pending head", provider: store.GreenhouseReviewProvider, verb: "done", data: map[string]string{"other": "value"}, wantReason: "invalid or missing greenhouse_pending_head"},
		{name: "incompatible admitted plan", provider: "builtin", verb: "done", data: map[string]string{"greenhouse_pending_head": strings.Repeat("a", 40)}, wantReason: "does not select Greenhouse full-pipeline"},
		{name: "changed task head", provider: store.GreenhouseReviewProvider, verb: "done", data: map[string]string{"greenhouse_pending_head": strings.Repeat("a", 40)}, wantReason: "Task head moved"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateWorking)
			freezeCILoopWithReview(t, f, 0, 0, "full-pipeline")
			plan, err := f.task.AdmittedPlan()
			if err != nil {
				t.Fatal(err)
			}
			plan.ReviewProvider = testCase.provider
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
			if testCase.verb != "" {
				if _, err := f.db.AddSignal(context.Background(), f.task.ID, testCase.verb, "fixture signal", testCase.data); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(f.home, f.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := f.service.reconcileGreenhouseRepositoryPrepublication(context.Background(), f.db, f.home, f.project, cfg); err != nil {
					t.Fatal(err)
				}
			}
			notices, err := f.db.Notices(context.Background(), f.project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(notices) != 1 || notices[0].Kind != "greenhouse_review_failed" || !strings.Contains(notices[0].Summary, testCase.wantReason) || !strings.Contains(notices[0].Summary, "posse review start t1") {
				t.Fatalf("skip Notice = %+v, want one Notice containing %q and a working next step", notices, testCase.wantReason)
			}
		})
	}
}

func TestGreenhouseWatchPassSpawnsAuthorizedReviewChild(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
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
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("review_provider = \"greenhouse\"\n[defaults]\nmerge_mode = \"pr\"\n"), 0o600); err != nil {
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
	taskScratch := os.Getenv("TMPDIR")
	if taskScratch == "" {
		taskScratch = t.TempDir()
	}
	shortDir, err := os.MkdirTemp(taskScratch, "gh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortDir) })
	shortHome := filepath.Join(shortDir, "p")
	if err := os.Symlink(f.home, shortHome); err != nil {
		t.Fatal(err)
	}
	f.home = shortHome
	f.service = testService(f.home, f.service.Herdr)
	greenhouseHome := greenhouse.HomeFor(f.home)
	t.Cleanup(func() {
		pid, live := greenhouse.LivePID(greenhouseHome)
		if live {
			if pid < 2 {
				t.Errorf("Greenhouse daemon returned unsafe PID %d", pid)
				return
			}
			if err := greenhouse.Stop(greenhouseHome, 30*time.Second); err != nil {
				t.Errorf("stop Greenhouse daemon %d: %v", pid, err)
			}
		}
		if pid, live := greenhouse.LivePID(greenhouseHome); live {
			t.Errorf("Greenhouse daemon %d is still alive after cleanup", pid)
		}
	})
	binDir, err := os.MkdirTemp(taskScratch, "gb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(binDir) })
	greenhouseBinary := os.Getenv("POSSE_TEST_GREENHOUSE_BINARY")
	realGreenhouse := greenhouseBinary != ""
	if !realGreenhouse {
		greenhouseBinary = "/usr/bin/false"
	}
	if realGreenhouse {
		if err := os.Symlink(greenhouseBinary, filepath.Join(binDir, "greenhouse")); err != nil {
			t.Fatal(err)
		}
		fakeReviewer := `#!/usr/bin/python3
import json, sys
if "--list-models" in sys.argv:
    print("provider model context")
    print("openai gpt-5.6-luna 128K")
    sys.exit(0)
request = json.load(sys.stdin)
path = request["Paths"][0]
head = request["Head"]
verdict = {"head": head, "verdict": "findings", "coverage": request["Paths"], "findings": [{"id": "watch-finding", "priority": "P1", "origin": "correctness", "file": path, "start": 1, "end": 1, "explanation": "Fixture review finding", "action": "Fix the reviewed issue", "rule": "", "head": head}]}
print(json.dumps({"type": "message_end", "message": {"role": "assistant", "provider": "openai", "model": "gpt-5.6-luna", "stopReason": "stop", "content": [{"type": "text", "text": json.dumps(verdict)}]}}))
print(json.dumps({"type": "agent_settled", "aborted": False}))
`
		if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(fakeReviewer), 0o700); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(filepath.Join(binDir, "greenhouse"), []byte("#!/bin/sh\nsleep 2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	binary := filepath.Join(f.root, "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = filepath.Clean(filepath.Join(workingDir, "../.."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Posse CLI: %v\\n%s", err, output)
	}
	t.Setenv("POSSE_TEST_HERDR", "1")
	t.Setenv("POSSE_TEST_DISABLE_SYSTEMD_WATCH", "1")
	if err := os.WriteFile(watchRuntimeLogPath(f.home, f.project), []byte("prior daemon run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	if err := f.service.startWatchRuntime(context.Background(), f.db, f.home, f.project, binary); err != nil {
		t.Fatal(err)
	}
	watch, err := f.db.WatchRuntime(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var process watchProcess
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		process, _ = inspectWatchProcess(f.home, watch.PID)
		if process.PID == watch.PID {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if process.PID != watch.PID {
		t.Fatalf("Project Watch PID %d did not start", watch.PID)
	}
	t.Cleanup(func() {
		if err := stopStoredWatchRuntime(context.Background(), f.home, process); err != nil {
			t.Errorf("stop Project Watch: %v", err)
		}
	})
	passToken := "greenhouse-watch-pass"
	if err := writeFile(watchPassRequestPath(f.home, f.project), []byte(passToken+"\n")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for readWatchPassToken(watchPassDonePath(f.home, f.project)) != passToken && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if readWatchPassToken(watchPassDonePath(f.home, f.project)) != passToken {
		t.Fatal("Project Watch did not serve the requested pass")
	}
	watchLog, err := os.ReadFile(watchRuntimeLogPath(f.home, f.project))
	if err != nil || !strings.Contains(string(watchLog), "prior daemon run") || !strings.Contains(string(watchLog), "Watch poll completed for shop") {
		t.Fatalf("watch.log after poll-only restart = %q, err=%v", watchLog, err)
	}
	if _, err := f.db.AddSignal(context.Background(), f.task.ID, "done", "Greenhouse review requested", map[string]string{"greenhouse_pending_head": head}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateDone, "worker", "Greenhouse review requested"); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	deadline = startedAt.Add(watchPassInterval + time.Second)
	ownerPath := parallelReviewOwnerPath(filepath.Join(f.home, "projects", f.project.Name, "tasks", taskIDString(f.task.Seq), "greenhouse"), f.project.Name)
	var owner *parallelReviewOwner
	for time.Now().Before(deadline) {
		owner, err = readParallelReviewOwner(ownerPath)
		if err != nil || owner != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || owner == nil || owner.PID < 2 {
		t.Fatalf("poll daemon did not start the Greenhouse review within one poll interval: owner=%+v err=%v", owner, err)
	}
	deadline = time.Now().Add(45 * time.Second)
	var requests []store.GreenhouseReviewRequest
	var notices []store.Notice
	for time.Now().Before(deadline) {
		requests, err = f.db.GreenhouseReviewRequests(context.Background(), f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		notices, err = f.db.Notices(context.Background(), f.project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if (len(requests) > 0 && (requests[0].Outcome == "verdict" || requests[0].Outcome == "failed")) || countNoticeKind(notices, "greenhouse_review_failed") > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(requests) != 1 || requests[0].RequestedHead != head {
		t.Fatalf("watch pass did not reach the spawned review child: requests=%+v notices=%+v", requests, notices)
	}
	if realGreenhouse {
		if requests[0].Outcome != "verdict" || requests[0].VerdictID == nil {
			t.Fatalf("real Greenhouse request = %+v, want a persisted, linked Verdict", requests[0])
		}
		deadline := time.Now().Add(15 * time.Second)
		var pending bool
		for time.Now().Before(deadline) {
			pending, err = f.db.ReviewFixInstructionPending(context.Background(), f.task.ID)
			if err != nil || pending {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		verdicts, err := f.db.VerdictsFor(context.Background(), f.task.ID)
		if err != nil || len(verdicts) != 1 {
			t.Fatalf("Greenhouse Verdicts = %+v, %v", verdicts, err)
		}
		verdict := verdicts[0]
		wantBase, err := greenhouseReviewBase(context.Background(), f.task.WorktreePath, verdict.Target, head)
		if err != nil || verdict.Head != head || verdict.ReviewBase != wantBase || len(verdict.ReviewBase) != 40 && len(verdict.ReviewBase) != 64 || verdict.Outcome != "findings" || verdict.Blocking < 1 {
			t.Fatalf("persisted Greenhouse Verdict = %+v, expected blocking findings at head=%s base=%s, err=%v", verdict, head, wantBase, err)
		}
		providerFindings, err := f.db.ProviderFindingsForTask(context.Background(), f.task.ID)
		if err != nil || len(providerFindings) != 1 || providerFindings[0].HeadSHA != head {
			t.Fatalf("persisted provider findings = %+v, err=%v", providerFindings, err)
		}
		if err != nil || !pending {
			notices, noticeErr := f.db.Notices(context.Background(), f.project.ID, false)
			messages, messageErr := f.db.PendingTaskInstructions(context.Background(), f.task.ID)
			t.Fatalf("Greenhouse finding did not queue a Fix Round: pending=%t err=%v notices=%+v noticeErr=%v messages=%+v messageErr=%v", pending, err, notices, noticeErr, messages, messageErr)
		}
		var fixRounds int
		if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='review'`, f.task.ID).Scan(&fixRounds); err != nil || fixRounds != 1 {
			t.Fatalf("queued review Fix Rounds = %d, err=%v", fixRounds, err)
		}
		t.Logf("E2E: done Signal head=%s -> Watch spawned _greenhouse review -> one real Greenhouse request and one findings Verdict (review_base_sha=%s) -> one queued Fix Round", head, verdict.ReviewBase)
	} else if requests[0].Outcome != "failed" {
		t.Fatalf("failed child request outcome = %q, want failed", requests[0].Outcome)
	}
	for _, notice := range notices {
		if notice.Kind == "greenhouse_review_failed" && strings.Contains(notice.Summary, "lead_only") {
			t.Fatalf("verified Watch child was rejected as Lead-only: %s", notice.Summary)
		}
	}
}

func TestPinGreenhouseReviewCheckoutIsScopedByRepository(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	checkouts := make(map[string]string)
	for _, repository := range []string{"shop", "other"} {
		member := greenhouseMember{name: repository, root: f.repo, worktree: f.worktree}
		pinned, err := pinGreenhouseReviewCheckout(context.Background(), f.db, f.home, f.project, f.task, member, f.headSHA)
		if err != nil {
			t.Fatal(err)
		}
		if actual, err := gitOutput(context.Background(), pinned.worktree, "rev-parse", "HEAD"); err != nil || actual != f.headSHA {
			t.Fatalf("%s checkout head = %q, want %s, err=%v", repository, actual, f.headSHA, err)
		}
		checkouts[repository] = pinned.worktree
	}
	if checkouts["shop"] == checkouts["other"] {
		t.Fatalf("repository members share review checkout %q", checkouts["shop"])
	}
}

func TestGreenhouseVerdictReplayReturnsPersistedHeadForPartialNotice(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	member := greenhouseMember{name: f.project.Name, root: f.task.WorktreePath, worktree: f.task.WorktreePath}
	result := greenhouse.RoundResult{Grant: "shop/t1/shop/r1", RunID: "replayed-partial", Result: &greenhouse.HostResult{
		Head: f.headSHA, Target: f.headSHA, Verdict: "clean",
	}}
	ctx := context.Background()
	if recorded, _, err := recordGreenhouseVerdict(ctx, f.db, f.task, member, t.TempDir(), "P1", config.Config{}, result); err != nil || !recorded {
		t.Fatalf("record initial Verdict: recorded=%t err=%v", recorded, err)
	}
	result.Result.Verdict = "partial"
	result.Result.Uncovered = []greenhouse.UncoveredPath{{Path: "large.sql", Reason: "hunk windows exceed 65536 bytes"}}
	recorded, verdict, err := recordGreenhouseVerdict(ctx, f.db, f.task, member, t.TempDir(), "P1", config.Config{}, result)
	if err != nil || recorded || verdict.Head != f.headSHA || verdict.Outcome != "clean" {
		t.Fatalf("replayed partial Verdict = recorded:%t verdict:%+v err:%v; want persisted clean Verdict for head %s", recorded, verdict, err, f.headSHA)
	}
	if _, err := f.db.CreateGreenhousePartialNoticeOnce(ctx, f.project.ID, f.task.ID, member.name, result.Result.Head, "Partial review", result.Result); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0].DataJSON, f.headSHA) {
		t.Fatalf("partial notice for replayed Verdict = %+v, %v; want reviewed head %s", notices, err, f.headSHA)
	}
}

func TestGreenhouseVerdictComputesBaseAndChecksProviderBase(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	member := greenhouseMember{name: f.project.Name, root: f.task.WorktreePath, worktree: f.task.WorktreePath}
	ctx := context.Background()
	base, err := greenhouseReviewBase(ctx, member.worktree, f.headSHA, f.headSHA)
	if err != nil {
		t.Fatal(err)
	}
	result := greenhouse.RoundResult{Grant: "shop/t1/shop/r1", RunID: "run-without-base", Result: &greenhouse.HostResult{Head: f.headSHA, Target: f.headSHA, Verdict: "findings"}}
	if recorded, verdict, err := recordGreenhouseVerdict(ctx, f.db, f.task, member, t.TempDir(), "P1", config.Config{}, result); err != nil || !recorded || verdict.ReviewBase != base {
		t.Fatalf("Verdict with Posse-computed base = %+v, %v", verdict, err)
	}
	result.Grant = "shop/t1/shop/r2"
	result.Result.ReviewBase = strings.Repeat("f", 40)
	if _, _, err := recordGreenhouseVerdict(ctx, f.db, f.task, member, t.TempDir(), "P1", config.Config{}, result); err == nil || !strings.Contains(err.Error(), "differs from Posse-computed") {
		t.Fatalf("mismatched Greenhouse review_base error = %v", err)
	}
	verdicts, err := f.db.VerdictsFor(ctx, f.task.ID)
	if err != nil || len(verdicts) != 1 || verdicts[0].ReviewBase != base {
		t.Fatalf("Greenhouse Verdicts = %+v, %v", verdicts, err)
	}
}

func TestParallelPendingGrantIdentifiesRequestForFailureRecording(t *testing.T) {
	pendingPath := filepath.Join(t.TempDir(), "request.pending.json")
	if err := os.WriteFile(pendingPath, []byte(`{"grant":"shop/t1/shop/r1","request":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	grant, err := parallelPendingGrant(pendingPath)
	if err != nil || grant != "shop/t1/shop/r1" {
		t.Fatalf("pending grant = %q, %v", grant, err)
	}
}

func TestReviewGreenhouseWithRetriesRetriesTargetFetchFailure(t *testing.T) {
	calls := 0
	result, attempts, err := reviewGreenhouseWithRetries(context.Background(), true, time.Second, func(time.Duration) (greenhouse.RoundResult, error) {
		calls++
		if calls == 1 {
			return greenhouse.RoundResult{}, errors.New("greenhouse_target_unavailable: could not fetch target origin/main")
		}
		return greenhouse.RoundResult{Outcome: greenhouse.Settled}, nil
	})
	if err != nil || calls != 2 || attempts != 2 || result.Outcome != greenhouse.Settled {
		t.Fatalf("retry result=%+v calls=%d attempts=%d err=%v; want same review to succeed after target fetch recovers", result, calls, attempts, err)
	}
}

func TestAutomaticGreenhouseReviewDoesNotRetrySizeFailures(t *testing.T) {
	for _, reason := range []string{
		"intent_too_long: intent is 8193 bytes; maximum is 8192",
		"prompt_exceeds_context (12000 tokens > 8192)",
		"context_overflow (12000 tokens > 8192)",
	} {
		t.Run(reason, func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateDone)
			ctx := context.Background()
			now := time.Now().UnixMilli()
			if err := f.db.RecordGreenhouseReviewRequest(ctx, store.GreenhouseReviewRequest{
				ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: f.headSHA,
				Provider: store.GreenhouseReviewProvider, RequestedAt: now - 1_000, SettledAt: now - 1, Outcome: "failed",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.CreateGreenhouseFailureNoticeOnce(ctx, f.project.ID, f.task.ID, f.project.Name, f.headSHA, reason); err != nil {
				t.Fatal(err)
			}
			starts := 0
			previous := selfExecCommand
			selfExecCommand = func(...string) (*exec.Cmd, error) {
				starts++
				return exec.Command("true"), nil
			}
			t.Cleanup(func() { selfExecCommand = previous })
			started := false
			if err := f.service.startParallelGreenhouseReviewsTracked(ctx, f.db, f.home, f.project, f.task, []store.ReviewTarget{{Repository: f.project.Name, Commit: f.headSHA}}, &started, true); err != nil {
				t.Fatal(err)
			}
			if started || starts != 0 {
				t.Fatalf("size-failure scheduler started=%t, child starts=%d; want no retry", started, starts)
			}
		})
	}
}

func TestAutomaticGreenhouseReviewRespectsRetryBackoff(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := f.db.RecordGreenhouseReviewRequest(ctx, store.GreenhouseReviewRequest{
		ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: f.headSHA,
		Provider: store.GreenhouseReviewProvider, RequestedAt: now - 1_000, SettledAt: now - 1, Outcome: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateGreenhouseFailureNoticeOnce(ctx, f.project.ID, f.task.ID, f.project.Name, f.headSHA, "temporary Greenhouse failure"); err != nil {
		t.Fatal(err)
	}
	started := false
	if err := f.service.startParallelGreenhouseReviewsTracked(ctx, f.db, f.home, f.project, f.task, []store.ReviewTarget{{Repository: f.project.Name, Commit: f.headSHA}}, &started, false); err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("automatic review started again before retry backoff expired")
	}
	requests, err := f.db.GreenhouseReviewRequests(ctx, f.project.ID)
	if err != nil || len(requests) != 1 {
		t.Fatalf("Greenhouse requests = %+v, %v; want only the original failed request", requests, err)
	}
}

func TestResumedGreenhouseGrantFailuresReachRetryLimit(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	ctx := context.Background()
	for attempt := range greenhouseReviewFailureLimit {
		at := int64(100 + attempt*10)
		if err := f.db.RecordGreenhouseReviewRequest(ctx, store.GreenhouseReviewRequest{
			ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: f.headSHA,
			Provider: store.GreenhouseReviewProvider, RequestedAt: 90, SettledAt: at, Outcome: "failed", Grant: "shop/t1/shop/r1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.CreateGreenhouseFailureNoticeOnce(ctx, f.project.ID, f.task.ID, f.project.Name, f.headSHA, "temporary Greenhouse failure"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.startParallelGreenhouseReviews(ctx, f.db, f.home, f.project, f.task, []store.ReviewTarget{{Repository: f.project.Name, Commit: f.headSHA}}); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := countNoticeKind(notices, "needs_decision"); got != 1 {
		t.Fatalf("needs_decision Notices = %d, want one after %d failed resumed attempts: %+v", got, greenhouseReviewFailureLimit, notices)
	}
}

func TestGreenhouseFailureAtRetryLimitRaisesNeedsDecisionNotice(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	ctx := context.Background()
	for attempt := range greenhouseReviewFailureLimit {
		at := int64(100 + attempt*10)
		if err := f.db.RecordGreenhouseReviewRequest(ctx, store.GreenhouseReviewRequest{
			ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: f.headSHA,
			Provider: store.GreenhouseReviewProvider, RequestedAt: at, SettledAt: at + 1, Outcome: "failed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.CreateGreenhouseFailureNoticeOnce(ctx, f.project.ID, f.task.ID, f.project.Name, f.headSHA, "Greenhouse binary is missing"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.startParallelGreenhouseReviews(ctx, f.db, f.home, f.project, f.task, []store.ReviewTarget{{Repository: f.project.Name, Commit: f.headSHA}}); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	needsDecision, failure := 0, 0
	for _, notice := range notices {
		if notice.Kind == "needs_decision" && strings.Contains(notice.Summary, "Greenhouse binary is missing") {
			needsDecision++
		}
		if notice.Kind == "greenhouse_review_failed" && notice.AckedAt > 0 {
			failure++
		}
	}
	if needsDecision != 1 || failure != 1 {
		t.Fatalf("retry-limit Notices = %+v, want one actionable needs_decision and one acknowledged failure Notice", notices)
	}
}

func TestRepeatedGreenhouseReviewFailuresAreBoundedByExactHead(t *testing.T) {
	requests := []store.GreenhouseReviewRequest{
		{TaskID: 1, Repository: "shop", RequestedHead: "head-a", Outcome: "failed"},
		{TaskID: 1, Repository: "shop", RequestedHead: "head-b", Outcome: "failed"},
		{TaskID: 2, Repository: "shop", RequestedHead: "head-a", Outcome: "failed"},
	}
	if got, _ := repeatedGreenhouseReviewFailures(requests, 1, "shop", "head-a"); got != 1 {
		t.Fatalf("failed requests for exact Task head = %d, want 1", got)
	}
	requests = append(requests, store.GreenhouseReviewRequest{TaskID: 1, Repository: "shop", RequestedHead: "head-a", Outcome: "failed"}, store.GreenhouseReviewRequest{TaskID: 1, Repository: "shop", RequestedHead: "head-a", Outcome: "failed"})
	if got, _ := repeatedGreenhouseReviewFailures(requests, 1, "shop", "head-a"); got < greenhouseReviewFailureLimit {
		t.Fatalf("same-head failures = %d, want limit %d", got, greenhouseReviewFailureLimit)
	}
}

func TestParallelReviewDoesNotRetryNonRetryableSizeErrors(t *testing.T) {
	for _, message := range []string{
		"intent_too_long: intent is 8193 bytes; maximum is 8192",
		"prompt_exceeds_context (12000 tokens > 8192)",
		"context_overflow (12000 tokens > 8192)",
	} {
		t.Run(message, func(t *testing.T) {
			calls := 0
			_, attempts, err := reviewGreenhouseWithRetries(context.Background(), true, time.Millisecond, func(time.Duration) (greenhouse.RoundResult, error) {
				calls++
				return greenhouse.RoundResult{}, errors.New(message)
			})
			if err == nil || calls != 1 || attempts != 1 {
				t.Fatalf("size-error review made %d calls and %d attempts, err=%v; want one terminal attempt", calls, attempts, err)
			}
		})
	}
}

func TestGreenhouseIntentFitsByteLimitAtUTF8Boundary(t *testing.T) {
	plan := &taskplan.Plan{Inputs: []taskplan.Input{{Role: "brief", Bytes: []byte("---\ntype: change\ntitle: \"" + strings.Repeat("é", 5000) + "\"\ndone_when: \"" + strings.Repeat("界", 5000) + "\"\n---\n")}}}
	intent := greenhouseIntent(store.Task{Title: "fallback"}, plan)
	if !utf8.ValidString(intent) {
		t.Fatalf("Greenhouse intent is not valid UTF-8")
	}
	if len(intent) > 8192 || !strings.Contains(intent, "[truncated]") {
		t.Fatalf("Greenhouse intent is %d bytes and truncation marker present=%t", len(intent), strings.Contains(intent, "[truncated]"))
	}
	if strings.TrimSpace(intent) == "" {
		t.Fatal("Greenhouse intent is blank")
	}
}

func TestGreenhouseIntentRemovesControlCharactersAndFallsBack(t *testing.T) {
	intent := greenhouseIntent(store.Task{Title: "A\x00B\rC\nD\tE"}, nil)
	if intent != "ABC\nD\tE" {
		t.Fatalf("sanitized Greenhouse intent = %q", intent)
	}
	if got := greenhouseIntent(store.Task{Title: "\x00\r\t\n"}, nil); got != "Greenhouse review" {
		t.Fatalf("blank Greenhouse intent fallback = %q", got)
	}
}

func TestParallelReviewRetriesTransportAsOneRequestObservation(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	ctx := context.Background()
	head, err := gitOutput(ctx, f.task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	grant := "shop/t1/shop/r1"
	calls := 0
	result, attempts, err := reviewGreenhouseWithRetries(ctx, true, time.Millisecond, func(time.Duration) (greenhouse.RoundResult, error) {
		calls++
		if calls == 1 {
			return greenhouse.RoundResult{}, errors.New("transport reset after request delivery")
		}
		return greenhouse.RoundResult{Outcome: greenhouse.Settled, Grant: grant}, nil
	})
	if err != nil || calls != 2 || attempts != 2 || result.Outcome != greenhouse.Settled {
		t.Fatalf("retried review = %+v after %d calls and %d attempts, %v", result, calls, attempts, err)
	}
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: grant, Head: head, Target: head, ReviewBase: head, Outcome: "clean", Mode: "parallel"}
	if recorded, err := f.db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return head, nil }); err != nil || !recorded {
		t.Fatalf("record final Verdict = %v, %v", recorded, err)
	}
	request := store.GreenhouseReviewRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: head, Provider: store.GreenhouseReviewProvider, RequestedAt: 100, SettledAt: 300, Outcome: "verdict", Grant: result.Grant}
	if err := f.db.RecordGreenhouseReviewRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	requests, err := f.db.GreenhouseReviewRequests(ctx, f.project.ID)
	if err != nil || len(requests) != 1 || requests[0].RequestedAt != 100 || requests[0].SettledAt != 300 {
		t.Fatalf("one retried request observation = %+v, %v", requests, err)
	}
}

func TestParallelReviewLockDetectsLiveAndAbandonedOwners(t *testing.T) {
	if lockPath := os.Getenv("POSSE_REVIEW_LOCK_CHILD"); lockPath != "" {
		lock, err := inheritedParallelReviewLock(3, lockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseParallelReviewLock(lock)
		ready, done := os.Getenv("POSSE_REVIEW_LOCK_READY"), os.Getenv("POSSE_REVIEW_LOCK_DONE")
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(done); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting for parent")
	}
	path := filepath.Join(t.TempDir(), "member.parallel.lock")
	first, busy, err := acquireParallelReviewLock(path)
	if err != nil || busy {
		t.Fatalf("acquire first lock = busy %v, err %v", busy, err)
	}
	if inFlight, err := parallelReviewInFlight(path); err != nil || !inFlight {
		t.Fatalf("active lock = %v, %v; want active", inFlight, err)
	}
	readyPath, donePath := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "done")
	command := exec.Command(os.Args[0], "-test.run=^TestParallelReviewLockDetectsLiveAndAbandonedOwners$")
	command.Env = append(os.Environ(), "POSSE_REVIEW_LOCK_CHILD="+path, "POSSE_REVIEW_LOCK_READY="+readyPath, "POSSE_REVIEW_LOCK_DONE="+donePath)
	command.ExtraFiles = []*os.File{first}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("child did not acquire inherited lock: %v", err)
	}
	if inFlight, err := parallelReviewInFlight(path); err != nil || !inFlight {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("inherited child lock = %v, %v; want active", inFlight, err)
	}
	if err := os.WriteFile(donePath, []byte("done"), 0o600); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("lock child: %v", err)
	}
	if inFlight, err := parallelReviewInFlight(path); err != nil || inFlight {
		t.Fatalf("released lock = %v, %v; want inactive", inFlight, err)
	}
}

func TestAbandonedParallelReviewResumesItsOriginalHeadBeforeCurrentHead(t *testing.T) {
	oldHead := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newHead := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	owner := &parallelReviewOwner{PID: 42, Head: oldHead, RequestedAt: 123}
	head, requestedAt := parallelRequestMetadata(newHead, 456, true, owner)
	if head != oldHead || requestedAt != 123 {
		t.Fatalf("resumed request = %s at %d, want old head %s at 123", head, requestedAt, oldHead)
	}
	head, requestedAt = parallelRequestMetadata(newHead, 789, false, owner)
	if head != newHead || requestedAt != 789 {
		t.Fatalf("new request = %s at %d, want current head %s at 789", head, requestedAt, newHead)
	}
}

func TestWorkspaceGreenhouseRecordsExactHeadEvenWhenHostMarksItNonCurrent(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "review_provider = \"greenhouse\"\n[defaults]\nmerge_mode = \"local\"\n", []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	repos, err := f.db.TaskRepos(ctx, f.task.ID)
	if err != nil || len(repos) != 1 {
		t.Fatalf("workspace Task repos = %+v, %v", repos, err)
	}
	plan, err := taskplan.Resolve(taskplan.Request{
		Brief:     taskplan.Select("brief", "workspace.md", []byte("---\ntype: change\ntitle: Greenhouse workspace\ndone_when: reviewed\nrepos: [worker]\n---\n")),
		Execution: taskplan.Execution{Mode: "sandbox", Scope: "workspace", Members: []taskplan.Member{{Name: "worker", Path: "worker", Base: "main", Target: "main", Delivery: "local", Review: "full-pipeline", ReviewProvider: store.GreenhouseReviewProvider}}},
		Launch:    taskplan.Launch{ProfileName: "test", Profile: config.Profile{Kind: "pi"}},
		Review:    "full-pipeline", ReviewProvider: store.GreenhouseReviewProvider, Delivery: "local",
		Autonomy:        config.Autonomy{Review: "ask", Merge: "ask"},
		Paths:           taskplan.Paths{Scratch: filepath.Join(f.home, "scratch", "stack", "t1"), Output: filepath.Join(f.home, "projects", "stack", "tasks", "t1")},
		ValidationLoops: &taskplan.ValidationLoops{Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1"}, ReviewProvider: store.GreenhouseReviewProvider},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT OR REPLACE INTO task_plan_revisions(task_id,revision,plan_json) VALUES(?,?,?)`, f.task.ID, plan.Revision, encoded); err != nil {
		t.Fatal(err)
	}
	f.task.PlanJSON = encoded
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("review_provider = \"greenhouse\"\n[review_loop]\ngreenhouse_timeout = \"1s\"\n[defaults]\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head, err := gitOutput(ctx, filepath.Join(f.task.WorktreePath, "worker"), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.service.greenhouseRound = func(_ context.Context, round greenhouse.Round) (greenhouse.RoundResult, error) {
		calls++
		grant := fmt.Sprintf("stack/t1/worker/r%d", calls)
		return greenhouse.RoundResult{
			Outcome: greenhouse.Settled, Grant: grant, RunID: "workspace-run",
			Result: &greenhouse.HostResult{Grant: grant, Head: head, Target: round.Target, Verdict: "findings", Current: false,
				Findings: []greenhouse.Finding{{ID: "finding-1", Priority: "P1", Origin: "correctness", File: "change.txt", Start: 1, End: 1, Explanation: "Finding", Action: "Fix it", Head: head}}},
		}, nil
	}
	binDir := t.TempDir()
	if err := os.Symlink("/usr/bin/true", filepath.Join(binDir, "greenhouse")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := f.db.AddSignal(ctx, f.task.ID, "done", "Workspace review requested", map[string]any{"greenhouse_pending_members": []greenhousePendingMember{{Repository: "worker", Head: head}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "merge_ready", Summary: "worker is ready", DataJSON: marshalJSON(map[string]any{"head_shas": map[string]string{"worker": head}})}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "merge_readiness_withdrawn", Summary: "prior workspace withdrawal", DataJSON: marshalJSON(map[string]string{"head_sha": head, "repo": "worker"})}); err != nil {
		t.Fatal(err)
	}
	decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{
		ProjectID: f.project.ID, TaskID: f.task.ID, Origin: "test:workspace-merge-ready", Kind: "merge_ready",
		Question: "Merge Task?", Options: []string{"merge", "wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := f.run("_greenhouse", "review", "t1", "--repo", "worker"); code != 0 {
		t.Fatalf("workspace Greenhouse review: %d\n%s", code, f.out.String())
	}
	requests, err := f.db.GreenhouseReviewRequests(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(requests) != 1 || requests[0].Outcome != "verdict" || requests[0].RequestedHead != head {
		t.Fatalf("workspace review calls=%d requests=%+v, want one exact-head Verdict", calls, requests)
	}
	verdicts, err := f.db.VerdictsFor(ctx, f.task.ID)
	if err != nil || len(verdicts) != 1 || verdicts[0].Head != head || verdicts[0].Outcome != "findings" {
		t.Fatalf("workspace Verdicts=%+v err=%v, want findings for %s", verdicts, err, head)
	}
	findings, err := f.db.ProviderFindingsForTask(ctx, f.task.ID)
	if err != nil || len(findings) != 1 || findings[0].Repository != "worker" || findings[0].HeadSHA != head {
		t.Fatalf("workspace provider findings=%+v err=%v, want one finding for %s", findings, err, head)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseWorkspacePrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	pending, err := f.db.ReviewFixInstructionPending(ctx, f.task.ID)
	if err != nil || !pending {
		t.Fatalf("workspace findings fix pending=%t err=%v, want queued repair", pending, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || countNoticeKind(notices, "greenhouse_review_failed") != 1 {
		t.Fatalf("workspace Greenhouse notices=%+v err=%v, want failure Notice", notices, err)
	}
	withdrawn := 0
	for _, notice := range notices {
		if notice.Kind == "merge_readiness_withdrawn" {
			withdrawn++
		}
	}
	if withdrawn != 1 {
		t.Fatalf("workspace merge readiness withdrawal count=%d notices=%+v, want one", withdrawn, notices)
	}
	pendingDecisions, err := f.db.PendingDecisions(ctx, f.project.ID)
	if err != nil || hasPendingDecision(pendingDecisions, decision.ID) {
		t.Fatalf("workspace stale merge-ready Decision remains pending: %+v, %v", pendingDecisions, err)
	}
}

func TestWorkspaceGreenhouseMovedPendingHeadRaisesNotice(t *testing.T) {
	f := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	worktree := filepath.Join(f.task.WorktreePath, "worker")
	pendingHead, err := gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "later.txt"), []byte("later work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "later.txt")
	gitTest(t, worktree, "commit", "-m", "move member head")
	if _, err := f.db.AddSignal(ctx, f.task.ID, "done", "Workspace head moved", map[string]any{"greenhouse_pending_members": []greenhousePendingMember{{Repository: "worker", Head: pendingHead}}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileGreenhouseWorkspacePrepublication(ctx, f.db, f.home, f.project, cfg); err != nil {
		t.Fatal(err)
	}
	requests, err := f.db.GreenhouseReviewRequests(ctx, f.project.ID)
	if err != nil || len(requests) != 0 {
		t.Fatalf("moved pending head requests=%+v err=%v, want no review request", requests, err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil || countNoticeKind(notices, "greenhouse_review_failed") != 1 {
		t.Fatalf("moved pending head notices=%+v err=%v, want one Greenhouse failure Notice", notices, err)
	}
	for _, notice := range notices {
		if notice.Kind == "greenhouse_review_failed" && strings.Contains(notice.Summary, "posse review start t1") {
			return
		}
	}
	t.Fatalf("moved pending head Notice has no working next step: %+v", notices)
}

func TestAbandonedParallelReviewKeepsItsHeadUntilAdapterResume(t *testing.T) {
	dir := t.TempDir()
	ownerPath := filepath.Join(dir, "shop.parallel-owner.json")
	pendingPath := filepath.Join(dir, "shop.pending.json")
	owner := parallelReviewOwner{PID: os.Getpid(), Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RequestedAt: 123}
	if err := writeParallelReviewOwner(ownerPath, owner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearParallelReviewOwner(ownerPath, pendingPath, owner.PID); err != nil {
		t.Fatal(err)
	}
	got, err := readParallelReviewOwner(ownerPath)
	if err != nil || got == nil || got.Head != owner.Head || got.RequestedAt != owner.RequestedAt {
		t.Fatalf("abandoned owner = %+v, %v; want original request metadata", got, err)
	}
	if gotAction := nextParallelReviewAction(false, true); gotAction != parallelReviewResume {
		t.Fatalf("abandoned pending action = %q, want resume", gotAction)
	}
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	if err := clearParallelReviewOwner(ownerPath, pendingPath, owner.PID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownerPath); !os.IsNotExist(err) {
		t.Fatalf("settled owner remains: %v", err)
	}
}
