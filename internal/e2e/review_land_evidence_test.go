//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
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
func resubmitChange(t *testing.T, f *prLifecycleFixture, change store.Task, note string) {
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
	if code, output := runReleaseCLI(t, f.binary, change.WorktreePath, env, "signal", "done", note, "--turn", turn); code != 0 {
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
