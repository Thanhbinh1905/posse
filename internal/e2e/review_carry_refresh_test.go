//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func advanceMainWithFile(t *testing.T, f *prLifecycleFixture, name string) {
	t.Helper()
	gitTest(t, f.env, f.repo, "switch", "main")
	if err := os.WriteFile(filepath.Join(f.repo, name), []byte(name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", name)
	gitTest(t, f.env, f.repo, "commit", "-m", "advance target with "+name)
	gitTest(t, f.env, f.repo, "push", "origin", "main")
}

func reviewVerdictRounds(t *testing.T, f *prLifecycleFixture, change store.Task) int {
	t.Helper()
	var rounds int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(DISTINCT round) FROM review_rounds WHERE ship_task_id=? AND status='verdict'`, change.ID).Scan(&rounds); err != nil {
		t.Fatal(err)
	}
	return rounds
}

func requireReviewCarry(t *testing.T, f *prLifecycleFixture, change store.Task, from, to string) {
	t.Helper()
	var carries int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=? AND from_head=? AND to_head=?`, change.ID, from, to).Scan(&carries); err != nil || carries != 1 {
		t.Fatalf("Review carry %s -> %s rows=%d err=%v", from, to, carries, err)
	}
}

func requireCarryNextStep(t *testing.T, output, from string) {
	t.Helper()
	if !strings.Contains(output, "review_carried_from") || !strings.Contains(output, from) || !strings.Contains(output, "posse validate test") {
		t.Fatalf("output does not tell the Worker the Review carried and validation is next: %s", output)
	}
}

// t715: the first candidate merged an advanced target, so it is no ancestor
// of the candidate refreshed for the next target. The unchanged author patch
// still carries the pass without a Review Round.
func TestFullPipelinePassCarriesAcrossCleanTargetRefreshOfMergedCandidate(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	for _, setting := range [][2]string{{"defaults.merge_mode", "pr"}, {"defaults.review", "full-pipeline"}, {"review_loop.enabled", "true"}} {
		runPosse(t, f.binary, f.repo, f.env, "config", "set", setting[0], setting[1])
	}
	if code, output := startReviewIdentityTask(t, f, "Target refresh carry", "author", ""); code != 0 {
		t.Fatalf("start Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "review-issue.txt"), []byte("validated operation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "review-issue.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "-m", "add reviewed change")
	advanceMainWithFile(t, f, "target-one.txt")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	publish := func() string {
		t.Helper()
		code, output := runReleaseCLI(t, f.binary, change.WorktreePath, workerEnv, "publish", "Candidate summary", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified", "--risk", "Risk: low\nRollback: revert")
		if code != 0 {
			t.Fatalf("submit candidate: %d %s", code, output)
		}
		return output
	}
	publish()
	passed, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if passed.CandidateSHA == passed.SubmittedSHA {
		t.Fatalf("first candidate %s did not merge the advanced target", passed.CandidateSHA)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, passed.WorktreePath, workerEnv, "signal", "done", "Candidate submitted for Review", "--turn", turn); code != 0 {
		t.Fatalf("request Review: %d %s", code, output)
	}
	recordPassingReview(t, f, "t1")
	deliverReviewPassNextStep(t, f, f.leadEnv, f.repo, change)

	advanceMainWithFile(t, f, "target-two.txt")
	if code, output := runReleaseCLI(t, f.binary, passed.WorktreePath, workerEnv, "signal", "working", "Refresh the candidate for the new target"); code != 0 {
		t.Fatalf("reopen Change Task: %d %s", code, output)
	}
	output := publish()
	refreshed, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil || refreshed.CandidateSHA == passed.CandidateSHA || refreshed.SubmittedSHA != passed.SubmittedSHA {
		t.Fatalf("candidate was not refreshed from the unchanged author head: %+v %v", refreshed, err)
	}
	requireReviewCarry(t, f, change, passed.CandidateSHA, refreshed.CandidateSHA)
	requireCarryNextStep(t, output, passed.CandidateSHA)
	if rounds := reviewVerdictRounds(t, f, change); rounds != 1 {
		t.Fatalf("target refresh counted as a Review Round: rounds=%d", rounds)
	}
	if code, output := runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "validate", "test", "--verify", "go test ./... -> passed", "--proof", "Intent behavior verified"); code != 0 {
		t.Fatalf("carried Review did not admit the test stage: %d %s", code, output)
	}
}

// t718: the User accepted a head at the Review cap (Decision 1001); a
// merge-only target refresh must carry that acceptance instead of raising a
// second review_cap Decision (1004).
func TestFullPipelineAcceptHeadCarriesAcrossCleanTargetRefresh(t *testing.T) {
	f := reviewIdentityFixture(t)
	changePath, candidatePath, changeSeq := startPrivateReviewChange(t, f, "1")
	recordBlockingReviewFinding(t, f, changeSeq)
	change := f.mustTask(t, changeSeq)
	if err := os.WriteFile(filepath.Join(candidatePath, "review-issue.txt"), []byte("changed after the final review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, candidatePath, "add", "review-issue.txt")
	gitTest(t, f.env, candidatePath, "commit", "-m", "unreviewed final candidate")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "working", "Submit the new candidate head"); code != 0 {
		t.Fatalf("reopen Change Task: %d %s", code, output)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "done", "Candidate changed after the final Review", "--turn", turn); code != 0 {
		t.Fatalf("record new private candidate head: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "decisions"); code != 0 || !strings.Contains(output, "review_cap") {
		t.Fatalf("reconcile Review cap Decision: %d %s", code, output)
	}
	var decisionID int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT id FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0`, change.ID).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	if code, output := runReviewIdentityCLI(t, f, "decide", fmt.Sprint(decisionID), "accept-head", "--user-approved", "Accept this exact candidate head."); code != 0 {
		t.Fatalf("record User's cap choice: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "apply", fmt.Sprint(decisionID)); code != 0 || !strings.Contains(output, "accepted-head") {
		t.Fatalf("apply exact-head acceptance: %d %s", code, output)
	}
	accepted, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	requireReviewPassNextStep(t, f, f.leadEnv, f.repo, change, candidatePath)

	advanceMainWithFile(t, f, "target-two.txt")
	if code, output := runReleaseCLI(t, f.binary, candidatePath, workerEnv, "signal", "working", "Refresh the accepted candidate for the new target"); code != 0 {
		t.Fatalf("resume Change Task: %d %s", code, output)
	}
	gitTest(t, f.env, changePath, "merge", "--ff-only", accepted.CandidateSHA)
	if code, output := runReleaseCLI(t, f.binary, changePath, workerEnv, "publish", "Refreshed candidate"); code == 0 || !strings.Contains(output, "candidate_author_moved") {
		t.Fatalf("moved author head did not stale the accepted candidate: %d %s", code, output)
	}
	code, output := runReleaseCLI(t, f.binary, changePath, workerEnv, "publish", "Refreshed candidate")
	if code != 0 {
		t.Fatalf("submit refreshed candidate: %d %s", code, output)
	}
	refreshed, err := f.db.LatestValidationCandidate(context.Background(), change.ID, "")
	if err != nil || refreshed.CandidateSHA == accepted.CandidateSHA {
		t.Fatalf("candidate was not refreshed: %+v %v", refreshed, err)
	}
	requireReviewCarry(t, f, change, accepted.CandidateSHA, refreshed.CandidateSHA)
	requireCarryNextStep(t, output, accepted.CandidateSHA)
	turn, err = f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, output = runReleaseCLI(t, f.binary, refreshed.WorktreePath, workerEnv, "signal", "done", "Candidate refreshed for the new target", "--turn", turn)
	if code != 0 {
		t.Fatalf("signal done on the refreshed candidate: %d %s", code, output)
	}
	requireCarryNextStep(t, output, accepted.CandidateSHA)
	runReviewIdentityCLI(t, f, "decisions")
	var openCaps int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0`, change.ID).Scan(&openCaps); err != nil || openCaps != 0 {
		t.Fatalf("merge-only target refresh raised %d new review_cap Decisions, err=%v", openCaps, err)
	}
}
