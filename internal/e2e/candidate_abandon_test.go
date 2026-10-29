//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// The clouddbv1 t32 sequence (#727): a full-pipeline candidate is promoted to
// the PR, the User changes scope and the Worker submits a newer candidate on
// top, then the User reverts the scope change and the Worker resets the Task
// branch to the promoted head. Merge must not deliver the promoted head while
// the newer candidate is pending, and abandoning the newer candidate returns
// the Task to the promoted head without a republish.
func TestFullPipelineAbandonedCandidateReturnsToPromotedHead(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.merge_mode", "pr")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", "full-pipeline")
	if code, output := startReviewIdentityTask(t, f, "Abandon candidate", "author", ""); code != 0 {
		t.Fatalf("start Change Task: %d %s", code, output)
	}
	ctx := context.Background()
	change := f.mustTask(t, "t1")
	workerEnv := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	workerEnv = setEnv(workerEnv, "POSSE_WORKER_HOME", f.home)
	worker := func(dir string, args ...string) (int, string) {
		t.Helper()
		return runReleaseCLI(t, f.binary, dir, workerEnv, args...)
	}
	turn := func() string {
		t.Helper()
		token, err := f.db.TaskTurnToken(ctx, change.ID)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	const prURL = "https://github.com/acme/shop/pull/17"

	promotedHead := commitCandidateFile(t, f, change.WorktreePath, "change.txt", "promoted scope\n")
	first := submitStagingCandidate(t, f, change, workerEnv, "", gitRef(t, f, f.remote, "refs/heads/main"))
	passCandidateReview(t, f, f.repo, change, "t2")
	recordCandidateStages(t, f, workerEnv, first, func(string) {})
	if code, output := worker(first.WorktreePath, "publish", "Staging candidate"); code != 0 || !strings.Contains(output, "promoted: candidate") {
		t.Fatalf("promote the first candidate: %d %s", code, output)
	}
	if code, output := worker(change.WorktreePath, "signal", "done", "Promoted", "--pr", prURL, "--turn", turn()); code != 0 {
		t.Fatalf("done Signal after promotion: %d %s", code, output)
	}
	if code, output := runFixtureCLI(t, f, f.repo, "merge", "t1"); code != 0 || !strings.Contains(output, "merging") {
		t.Fatalf("Merge readiness of the promoted head: %d %s", code, output)
	}

	// The User changes scope; the Worker submits a newer candidate on top.
	if code, output := worker(change.WorktreePath, "signal", "working", "Scope changed"); code != 0 {
		t.Fatalf("reopen for the scope change: %d %s", code, output)
	}
	scopedHead := commitCandidateFile(t, f, change.WorktreePath, "scope.txt", "changed scope\n")
	if code, output := worker(change.WorktreePath, "publish", "Scoped candidate", "--verify", "go test ./... -> passed", "--proof", "Scope verified", "--risk", "Risk: low\nRollback: revert"); code != 0 {
		t.Fatalf("submit the scoped candidate: %d %s", code, output)
	}
	scoped, err := f.db.LatestValidationCandidate(ctx, change.ID, "")
	if err != nil || scoped.State != store.ValidationCandidateActive || scoped.SubmittedSHA != scopedHead {
		t.Fatalf("scoped candidate=%+v err=%v", scoped, err)
	}
	if code, output := worker(scoped.WorktreePath, "signal", "done", "Requesting Review of the scoped candidate", "--turn", turn()); code != 0 {
		t.Fatalf("request Review of the scoped candidate: %d %s", code, output)
	}

	// The PR still carries the promoted head and turns green: neither
	// merge_ready nor Merge may deliver it while the scoped candidate pends.
	f.writeGraphQL(t, "pr1", "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", promotedHead)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	var ready int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='merge_ready'`, change.ID).Scan(&ready); err != nil || ready != 0 {
		t.Fatalf("merge_ready Notices=%d (%v) while candidate %s is pending", ready, err, scopedHead)
	}
	for _, args := range [][]string{{"merge", "t1"}, {"merge", "t1", "--execute", "--user-approved", "Merge it"}} {
		code, output := runFixtureCLI(t, f, f.repo, args...)
		if code == 0 || !strings.Contains(output, "candidate_pending") || !strings.Contains(output, scopedHead[:12]) || !strings.Contains(output, promotedHead[:12]) {
			t.Fatalf("%v with a pending candidate: %d %s", args, code, output)
		}
	}

	// The User reverts the scope change; the Worker resets to the promoted head.
	if code, output := worker(change.WorktreePath, "signal", "working", "Scope reverted"); code != 0 {
		t.Fatalf("reopen for the scope revert: %d %s", code, output)
	}
	gitTest(t, f.env, change.WorktreePath, "reset", "--hard", promotedHead)
	if code, output := worker(change.WorktreePath, "signal", "done", "Scope reverted", "--pr", prURL, "--turn", turn()); code == 0 || !strings.Contains(output, "candidate_author_moved") || !strings.Contains(output, "posse candidate abandon") {
		t.Fatalf("done Signal before abandoning the scoped candidate: %d %s", code, output)
	}
	if code, output := worker(change.WorktreePath, "candidate", "abandon", "t1", "--reason", "User reverted the scope change"); code != 0 || !strings.Contains(output, promotedHead) {
		t.Fatalf("abandon the scoped candidate: %d %s", code, output)
	}
	abandoned, err := f.db.ValidationCandidates(ctx, change.ID)
	if err != nil || len(abandoned) != 2 || abandoned[0].State != store.ValidationCandidatePromoted || abandoned[1].State != store.ValidationCandidateDiscarded {
		t.Fatalf("candidates after abandonment=%+v err=%v", abandoned, err)
	}
	if archived := gitRef(t, f, change.WorktreePath, abandoned[1].RefName); archived != scopedHead {
		t.Fatalf("abandoned candidate archive ref %s=%s, want %s", abandoned[1].RefName, archived, scopedHead)
	}
	var history string
	if err := f.db.QueryRowContext(ctx, `SELECT note FROM transitions WHERE task_id=? ORDER BY id DESC LIMIT 1`, change.ID).Scan(&history); err != nil || !strings.Contains(history, "Abandoned candidate") || !strings.Contains(history, "User reverted the scope change") {
		t.Fatalf("Task history=%q err=%v, want the abandonment", history, err)
	}
	if code, output := worker(change.WorktreePath, "signal", "done", "Scope reverted", "--pr", prURL, "--turn", turn()); code != 0 {
		t.Fatalf("done Signal after abandoning the scoped candidate: %d %s", code, output)
	}

	// Merge now names and delivers the promoted head.
	if code, output := runFixtureCLI(t, f, f.repo, "merge", "t1"); code != 0 || !strings.Contains(output, promotedHead) {
		t.Fatalf("Merge readiness after abandonment: %d %s", code, output)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	var summary string
	if err := f.db.QueryRowContext(ctx, `SELECT summary FROM notices WHERE task_id=? AND kind='merge_ready' ORDER BY id DESC LIMIT 1`, change.ID).Scan(&summary); err != nil || !strings.Contains(summary, promotedHead[:12]) {
		t.Fatalf("merge_ready summary=%q err=%v, want the merged head", summary, err)
	}
	if code, output := runFixtureCLI(t, f, f.repo, "merge", "t1", "--execute", "--user-approved", "Merge it"); code != 0 || !strings.Contains(output, promotedHead) {
		t.Fatalf("Merge after abandonment: %d %s", code, output)
	}
}
