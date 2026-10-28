//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// startQuickLoopChange admits a quick-review Change Task with an enabled Review
// Loop and completes it with one committed candidate.
func startQuickLoopChange(t *testing.T, f *prLifecycleFixture, autoStart string) store.Task {
	t.Helper()
	stopBackgroundWatch(t, f)
	for _, setting := range [][2]string{{"defaults.review", "quick"}, {"review_loop.enabled", "true"}, {"review_loop.auto_start", autoStart}} {
		runPosse(t, f.binary, f.repo, f.env, "config", "set", setting[0], setting[1])
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.yolo", "true", "--project", "shop")
	if code, output := startReviewIdentityTask(t, f, "Quick loop candidate", "author", ""); code != 0 {
		t.Fatalf("start Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	commitFile(t, f, change, "candidate.txt", "candidate change")
	signalReviewPolicyTask(t, f, change)
	return f.mustTask(t, "t1")
}

// resubmitChange reopens the done Change Task and reports its current branch
// tip done again, as a Worker does after a follow-up commit.
func resubmitChange(t *testing.T, f *prLifecycleFixture, change store.Task, note string, doneArgs ...string) {
	t.Helper()
	env := setEnv(f.env, "HERDR_PANE_ID", change.PaneID)
	env = setEnv(env, "POSSE_WORKER_HOME", f.home)
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, env, "signal", "working", note); code != 0 {
		t.Fatalf("reopen Change Task: %d %s", code, output)
	}
	turn, err := f.db.TaskTurnToken(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, env, append([]string{"signal", "done", note, "--turn", turn}, doneArgs...)...); code != 0 {
		t.Fatalf("complete follow-up: %d %s", code, output)
	}
}

func reviewRoundCount(t *testing.T, f *prLifecycleFixture, change store.Task) int {
	t.Helper()
	rounds, err := f.db.ReviewRoundsFor(context.Background(), change.ID)
	if err != nil {
		t.Fatal(err)
	}
	return len(rounds)
}

func TestCIFixAfterReviewPassHasNoReviewEvidence(t *testing.T) {
	f := reviewIdentityFixture(t)
	change := startQuickLoopChange(t, f, "false")
	recordPassingReview(t, f, "t1")
	passed := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	// A CI fix is an author change: it changes the tree the pass reviewed.
	commitFile(t, f, change, "ci-fix.txt", "fix failing CI")
	resubmitChange(t, f, change, "Fixed failing CI")
	fixed := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	before := gitTest(t, f.env, f.repo, "rev-parse", "main")
	for _, args := range [][]string{{"merge", "t1"}, {"merge", "t1", "--execute"}} {
		if code, output := runReviewIdentityCLI(t, f, args...); code != 1 || !strings.Contains(output, `"review_required"`) {
			t.Fatalf("posse %s admitted a CI fix after the pass without evidence: %d %s", strings.Join(args, " "), code, output)
		}
	}
	var carries int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=? AND to_head=?`, change.ID, fixed).Scan(&carries); err != nil || carries != 0 {
		t.Fatalf("CI fix %s received carried evidence from %s: rows=%d err=%v", fixed, passed, carries, err)
	}
	if got := gitTest(t, f.env, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("refused Merge changed main")
	}
	if got := f.mustTask(t, "t1"); got.State != store.StateDone {
		t.Fatalf("Task without evidence left done: %s", got.State)
	}
}

func TestCleanTargetMergeAfterReviewPassLandsWithoutANewRound(t *testing.T) {
	f := reviewIdentityFixture(t)
	change := startQuickLoopChange(t, f, "true")
	recordPassingReview(t, f, "t1")
	passed := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	// The target advances with an unrelated file; the Worker merges it in.
	if err := os.WriteFile(filepath.Join(f.repo, "target-update.txt"), []byte("target update\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", "target-update.txt")
	gitTest(t, f.env, f.repo, "commit", "-m", "advance target")
	gitTest(t, f.env, f.repo, "push", "origin", "main")
	gitTest(t, f.env, change.WorktreePath, "merge", "--no-edit", "main")
	merged := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	resubmitChange(t, f, change, "Merged the current target")
	var carries int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=? AND from_head=? AND to_head=?`, change.ID, passed, merged).Scan(&carries); err != nil || carries != 1 {
		t.Fatalf("clean target merge carry rows=%d err=%v", carries, err)
	}
	if code, output := runReviewIdentityCLI(t, f, "merge", "t1"); code != 0 || !strings.Contains(output, "merging") {
		t.Fatalf("carried pass did not open Merge: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLIWithResourceConflictRetry(t, f, "merge", "t1", "--execute"); code != 0 {
		t.Fatalf("carried pass did not land: %d %s", code, output)
	}
	if got := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "main")); got != merged {
		t.Fatalf("main=%s, want landed candidate %s", got, merged)
	}
	if rounds := reviewRoundCount(t, f, change); rounds != 1 {
		t.Fatalf("clean target merge started a new Review Round: rounds=%d", rounds)
	}
}

func TestBlockingReviewFindingRefusesMergeUnderReviewNone(t *testing.T) {
	f := reviewIdentityFixture(t)
	stopBackgroundWatch(t, f)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.enabled", "true")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.yolo", "true", "--project", "shop")
	if code, output := startReviewIdentityTask(t, f, "Unreviewed policy", "author", ""); code != 0 {
		t.Fatalf("start Change Task: %d %s", code, output)
	}
	change := f.mustTask(t, "t1")
	commitFile(t, f, change, "review-issue.txt", "unsafe operation")
	signalReviewPolicyTask(t, f, change)
	if plan, err := change.AdmittedPlan(); err != nil || plan == nil || plan.Review != "none" {
		t.Fatalf("Change Task was not admitted with review none: %#v %v", plan, err)
	}
	recordBlockingReviewFinding(t, f, "t1")
	before := gitTest(t, f.env, f.repo, "rev-parse", "main")
	for _, args := range [][]string{{"merge", "t1"}, {"merge", "t1", "--execute"}} {
		if code, output := runReviewIdentityCLI(t, f, args...); code != 1 || !strings.Contains(output, `"review_findings_open"`) {
			t.Fatalf("posse %s ignored a blocking finding under review none: %d %s", strings.Join(args, " "), code, output)
		}
	}
	if got := gitTest(t, f.env, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("refused Merge changed main")
	}
	const quote = "Accept the unsafe operation finding for this Task."
	if code, output := runReviewIdentityCLI(t, f, "review", "dispose", "t1", "P-1", "accept", "--user-approved", quote); code != 0 {
		t.Fatalf("record User acceptance: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLIWithResourceConflictRetry(t, f, "merge", "t1", "--execute", "--user-approved", "Merge this accepted Task"); code != 0 {
		t.Fatalf("User-accepted finding still refused Merge: %d %s", code, output)
	}
	if got := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "main")); got != strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD")) {
		t.Fatal("accepted Task did not land")
	}
}

// advanceTarget commits one file on the frozen target and pushes it to origin.
func advanceTarget(t *testing.T, f *prLifecycleFixture, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "add", name)
	gitTest(t, f.env, f.repo, "commit", "-m", "advance target")
	gitTest(t, f.env, f.repo, "push", "origin", "main")
}

func TestPRCarriedPassLandsPinnedToTheCarriedHead(t *testing.T) {
	f := reviewEvidenceFixture(t, "quick")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.enabled", "true")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.auto_start", "true")
	change := publishCandidate(t, f, "Carried PR change", "author", 17)
	recordPassingReview(t, f, "t1")
	passed := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	advanceTarget(t, f, "target-update.txt", "target update\n")
	gitTest(t, f.env, change.WorktreePath, "merge", "--no-edit", "main")
	gitTest(t, f.env, change.WorktreePath, "push", "origin", change.Branch)
	merged := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	resubmitChange(t, f, change, "Merged the current target", "--pr", "https://github.com/acme/shop/pull/17")
	var carries int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=? AND from_head=? AND to_head=?`, change.ID, passed, merged).Scan(&carries); err != nil || carries != 1 {
		t.Fatalf("clean target merge carry rows=%d err=%v", carries, err)
	}
	if code, out := runReviewIdentityCLI(t, f, "merge", "t1"); code != 0 || !strings.Contains(out, "merging") {
		t.Fatalf("carried pass did not open Merge: %d %s", code, out)
	}
	if got := f.mustTask(t, "t1").GatedSHA; got != merged {
		t.Fatalf("gated %s, want carried head %s", got, merged)
	}
	// Green CI for the passed head never qualifies the carried head.
	observePR(t, f, change, 17, "OPEN", "SUCCESS", passed, "")
	requireRefusal(t, f, "checks_not_green", "merge", "t1", "--execute")
	observePR(t, f, change, 17, "OPEN", "SUCCESS", merged, "")
	if code, out := runReviewIdentityCLI(t, f, "merge", "t1", "--execute"); code != 0 {
		t.Fatalf("aligned gate, carry, observation and forge head refused: %d %s", code, out)
	}
	calls, err := os.ReadFile(f.ghLog)
	if err != nil || strings.Count(string(calls), "pr merge") != 1 || !strings.Contains(string(calls), "--match-head-commit "+merged) {
		t.Fatalf("merge was not pinned to the carried head: %v\n%s", err, calls)
	}
	if rounds := reviewRoundCount(t, f, change); rounds != 1 {
		t.Fatalf("clean target merge started a new Review Round: rounds=%d", rounds)
	}
}

func TestPRHandResolvedTargetMergeAfterPassHasNoCarry(t *testing.T) {
	f := reviewEvidenceFixture(t, "quick")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.enabled", "true")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.auto_start", "false")
	change := publishCandidate(t, f, "Conflicted PR change", "author", 17)
	recordPassingReview(t, f, "t1")
	// The target edits the candidate's file, so merging it needs a hand resolution.
	advanceTarget(t, f, "candidate-t1.txt", "target version\n")
	if code, out := runReleaseCLI(t, "git", change.WorktreePath, f.env, "merge", "--no-edit", "main"); code == 0 {
		t.Fatalf("target merge did not conflict: %s", out)
	}
	if err := os.WriteFile(filepath.Join(change.WorktreePath, "candidate-t1.txt"), []byte("hand resolved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, change.WorktreePath, "add", "candidate-t1.txt")
	gitTest(t, f.env, change.WorktreePath, "commit", "--no-edit")
	gitTest(t, f.env, change.WorktreePath, "push", "origin", change.Branch)
	resolved := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	resubmitChange(t, f, change, "Merged the current target by hand", "--pr", "https://github.com/acme/shop/pull/17")
	requireRefusal(t, f, "review_required", "merge", "t1")
	var carries int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM review_carries WHERE ship_task_id=?`, change.ID).Scan(&carries); err != nil || carries != 0 {
		t.Fatalf("hand-resolved merge %s received a carry: rows=%d err=%v", resolved, carries, err)
	}
	if got := f.mustTask(t, "t1"); got.State != store.StateDone || got.GatedSHA != "" && got.GatedSHA != resolved {
		t.Fatalf("Task without evidence advanced: %s gated=%s", got.State, got.GatedSHA)
	}
}

// acceptCappedHead caps a one-round quick Review Loop on an unreviewed head and
// applies the User's accept-head answer to its review_cap Decision.
func acceptCappedHead(t *testing.T, f *prLifecycleFixture) (store.Task, string) {
	t.Helper()
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.max_rounds", "1")
	// Merge auto-starts the next Review Round only with an eligible reviewer.
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "dispatch.default.use", "reviewer")
	change := startQuickLoopChange(t, f, "true")
	recordPassingReview(t, f, "t1")
	commitFile(t, f, change, "after-review.txt", "change after the last Review Round")
	resubmitChange(t, f, change, "Changed after the last Review Round")
	head := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	requireRefusal(t, f, "review_round_cap", "merge", "t1")
	if code, output := runReviewIdentityCLI(t, f, "decisions"); code != 0 || !strings.Contains(output, "review_cap") {
		t.Fatalf("reconcile Review cap Decision: %d %s", code, output)
	}
	var decisionID int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT id FROM decisions WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0`, change.ID).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(decisionID, 10)
	if code, output := runReviewIdentityCLI(t, f, "decide", id, "accept-head", "--user-approved", "Accept this head."); code != 0 {
		t.Fatalf("record accept-head: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLI(t, f, "apply", id); code != 0 || !strings.Contains(output, "accepted-head") {
		t.Fatalf("apply accept-head: %d %s", code, output)
	}
	return change, head
}

func TestReviewCapAcceptHeadLetsMergeProceed(t *testing.T) {
	f := reviewIdentityFixture(t)
	change, head := acceptCappedHead(t, f)
	if code, output := runReviewIdentityCLI(t, f, "merge", "t1"); code != 0 || !strings.Contains(output, "merging") {
		t.Fatalf("accepted head did not open Merge: %d %s", code, output)
	}
	if code, output := runReviewIdentityCLIWithResourceConflictRetry(t, f, "merge", "t1", "--execute"); code != 0 {
		t.Fatalf("accepted head did not land: %d %s", code, output)
	}
	if got := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "main")); got != head {
		t.Fatalf("main=%s, want accepted head %s", got, head)
	}
	if rounds := reviewRoundCount(t, f, change); rounds != 1 {
		t.Fatalf("accepted head started a new Review Round: rounds=%d", rounds)
	}
}

func TestReviewCapAcceptHeadDoesNotCoverALaterCommit(t *testing.T) {
	f := reviewIdentityFixture(t)
	change, _ := acceptCappedHead(t, f)
	commitFile(t, f, change, "after-accept.txt", "change after accept-head")
	resubmitChange(t, f, change, "Changed after accept-head")
	before := gitTest(t, f.env, f.repo, "rev-parse", "main")
	requireRefusal(t, f, "review_round_cap", "merge", "t1")
	if got := gitTest(t, f.env, f.repo, "rev-parse", "main"); got != before {
		t.Fatal("refused Merge changed main")
	}
	if got := f.mustTask(t, "t1"); got.State != store.StateDone {
		t.Fatalf("unaccepted head left done: %s", got.State)
	}
}
