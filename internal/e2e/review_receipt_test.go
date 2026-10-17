//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// reviewEvidenceFixture drives Ship Tasks by hand on PR delivery: the fake
// agents stay idle, so each test commits, publishes and signals explicitly.
func reviewEvidenceFixture(t *testing.T, review string) *prLifecycleFixture {
	t.Helper()
	f := reviewIdentityFixture(t)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.landing_mode", "pr")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.review", review)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "autonomy.yolo", "true", "--project", "shop")
	return f
}

// publishCandidate commits on the Ship branch, publishes it as PR number and
// reports done, as a Rider would.
func publishCandidate(t *testing.T, f *prLifecycleFixture, title, profile string, number int) store.Task {
	t.Helper()
	if code, out := rideReviewIdentityTask(t, f, title, profile, ""); code != 0 {
		t.Fatalf("ride %s: %d %s", title, code, out)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	ship := tasks[len(tasks)-1]
	commitFile(t, f, ship, "candidate-"+taskIDOf(ship)+".txt", "candidate "+title)
	env := setEnv(f.env, "HERDR_PANE_ID", ship.PaneID)
	env = setEnv(env, "POSSE_WORKER_HOME", f.home)
	runPosse(t, f.binary, ship.WorktreePath, env, "publish", "Candidate "+title)
	signalReviewPolicyTask(t, f, ship, "--pr", "https://github.com/acme/shop/pull/"+strconv.Itoa(number))
	return f.mustTask(t, taskIDOf(ship))
}

// commitFile commits a real change: squash merges drop empty commits.
func commitFile(t *testing.T, f *prLifecycleFixture, task store.Task, name, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(task.WorktreePath, name), []byte(message+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, task.WorktreePath, "add", name)
	gitTest(t, f.env, task.WorktreePath, "commit", "-m", message)
}

func taskIDOf(task store.Task) string { return "t" + strconv.Itoa(task.Seq) }

// observePR writes the fake forge observation for one watched Task and polls.
func observePR(t *testing.T, f *prLifecycleFixture, task store.Task, number int, state, checks, head, mergeCommit string) {
	t.Helper()
	pull := map[string]any{
		"url": "https://github.com/acme/shop/pull/" + strconv.Itoa(number), "state": state, "headRefOid": head, "baseRefName": "main",
		"mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "mergeCommit": map[string]any{"oid": mergeCommit},
		"statusCheckRollup": map[string]any{"state": checks, "contexts": map[string]any{"nodes": []any{}}},
	}
	encoded, err := json.Marshal(map[string]any{"data": map[string]any{"repo1": map[string]any{"pr" + strconv.Itoa(task.Seq): pull}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // pr_poll is 1ms; the next show must poll.
	if code, out := runReviewIdentityCLI(t, f, "show", taskIDOf(task)); code != 0 && !strings.Contains(out, "teardown_in_progress") {
		t.Fatalf("show: %d %s", code, out)
	}
	if !waitForCondition(10*time.Second, func() bool {
		observation, err := f.db.LatestPRObservation(context.Background(), task.ID)
		return err == nil && observation.HeadSHA == head && observation.State == state
	}) {
		t.Fatalf("observation of %s at %s was not recorded", state, head)
	}
}

func requireRefusal(t *testing.T, f *prLifecycleFixture, code string, args ...string) {
	t.Helper()
	exit, out := runReviewIdentityCLI(t, f, args...)
	if exit != 1 || !strings.Contains(out, `"`+code+`"`) {
		t.Fatalf("posse %s: want %s, got %d %s", strings.Join(args, " "), code, exit, out)
	}
	if _, err := os.Stat(f.ghMerge); !os.IsNotExist(err) {
		t.Fatalf("refused %s reached the forge merge: %v", code, err)
	}
}

func passReview(t *testing.T, f *prLifecycleFixture, ship store.Task, title string) {
	t.Helper()
	if code, out := rideReviewIdentityTask(t, f, title, "reviewer", taskIDOf(ship)); code != 0 {
		t.Fatalf("review: %d %s", code, out)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	review := tasks[len(tasks)-1]
	report := filepath.Join(f.home, "scratch", "shop", taskIDOf(review), "report.md")
	if err := os.WriteFile(report, []byte("No findings.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--verdict", "pass")
}

func TestPRMergeBindsHeadObservationAndLocalVerdict(t *testing.T) {
	f := reviewEvidenceFixture(t, "local")
	ship := publishCandidate(t, f, "PR lifecycle change", "author", 17)
	requireRefusal(t, f, "review_required", "land", "t1")
	passReview(t, f, ship, "First review")
	if code, out := runReviewIdentityCLI(t, f, "land", "t1"); code != 0 || !strings.Contains(out, "landing") {
		t.Fatalf("reviewed PR did not open Land: %d %s", code, out)
	}
	reviewed := f.mustTask(t, "t1").GatedSHA
	observePR(t, f, ship, 17, "OPEN", "SUCCESS", reviewed, "")

	// The Rider pushes an unreviewed commit; CI turns green for the new head.
	commitFile(t, f, ship, "follow-up.txt", "unreviewed follow-up")
	gitTest(t, f.env, ship.WorktreePath, "push", "origin", ship.Branch)
	moved := strings.TrimSpace(gitTest(t, f.env, ship.WorktreePath, "rev-parse", "HEAD"))
	observePR(t, f, ship, 17, "OPEN", "SUCCESS", moved, "")
	requireRefusal(t, f, "branch_moved", "land", "t1", "--merge")
	// Re-gating the new head finds the pass bound to the older head only.
	requireRefusal(t, f, "review_required", "land", "t1")
	if got := f.mustTask(t, "t1"); got.State != store.StateDone {
		t.Fatalf("stale evidence advanced the Task: %s", got.State)
	}

	passReview(t, f, f.mustTask(t, "t1"), "Second review")
	if code, out := runReviewIdentityCLI(t, f, "land", "t1"); code != 0 {
		t.Fatalf("current pass refused: %d %s", code, out)
	}
	if got := f.mustTask(t, "t1").GatedSHA; got != moved {
		t.Fatalf("gated %s, want %s", got, moved)
	}
	// A green observation of the older head never qualifies the new head.
	observePR(t, f, ship, 17, "OPEN", "SUCCESS", reviewed, "")
	requireRefusal(t, f, "checks_not_green", "land", "t1", "--merge")
	observePR(t, f, ship, 17, "OPEN", "SUCCESS", moved, "")
	if code, out := runReviewIdentityCLI(t, f, "land", "t1", "--merge"); code != 0 {
		t.Fatalf("aligned head, observation and verdict refused: %d %s", code, out)
	}
	calls, err := os.ReadFile(f.ghLog)
	if err != nil || strings.Count(string(calls), "pr merge") != 1 || !strings.Contains(string(calls), "--match-head-commit "+moved) {
		t.Fatalf("merge was not pinned to the reviewed head: %v\n%s", err, calls)
	}
	merge := f.mergeOnLocalOrigin(t, f.mustTask(t, "t1"), "t1")
	observePR(t, f, ship, 17, "MERGED", "SUCCESS", moved, merge)
	waitLanded(t, f, "t1", merge)
	requireNoNotice(t, f, "t1", "review_policy_violation")
}

func TestExternalMergeWithoutEvidenceIsPolicyViolation(t *testing.T) {
	f := reviewEvidenceFixture(t, "local")
	ship := publishCandidate(t, f, "External merge change", "author", 17)
	stopBackgroundLookout(t, f)
	merge := f.mergeOnLocalOrigin(t, ship, "t1")
	observePR(t, f, ship, 17, "MERGED", "SUCCESS", strings.TrimSpace(gitTest(t, f.env, ship.WorktreePath, "rev-parse", "HEAD")), merge)
	waitLanded(t, f, "t1", merge)
	f.requireNotice(t, "t1", "pr_merged")
	f.requireNotice(t, "t1", "review_policy_violation")
	verdicts, err := f.db.ReviewVerdictsFor(context.Background(), ship.ID)
	if err != nil || len(verdicts) != 0 {
		t.Fatalf("external merge fabricated a verdict: %#v %v", verdicts, err)
	}
	if _, err := os.Stat(f.ghMerge); !os.IsNotExist(err) {
		t.Fatalf("Posse issued a merge: %v", err)
	}
}

// waitLanded accepts landed or the automatic Teardown that follows a merge.
func waitLanded(t *testing.T, f *prLifecycleFixture, taskID, merge string) {
	t.Helper()
	if !waitForCondition(30*time.Second, func() bool {
		task := f.mustTask(t, taskID)
		return (task.State == store.StateLanded || task.State == store.StateTornDown) && task.LandedRef == merge
	}) {
		t.Fatalf("Task %s did not Land at %s: %#v", taskID, merge, f.mustTask(t, taskID))
	}
}

func requireNoNotice(t *testing.T, f *prLifecycleFixture, taskID, kind string) {
	t.Helper()
	task := f.mustTask(t, taskID)
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == kind {
			t.Fatalf("unexpected %s Notice: %#v", kind, notice)
		}
	}
}

// installFakeNoMistakes serves only the read-only receipt query from a file.
// Any other subcommand fails, so the test proves Posse never drives a run.
func installFakeNoMistakes(t *testing.T, f *prLifecycleFixture) (receipt, log string) {
	t.Helper()
	receipt, log = filepath.Join(f.root, "nm-receipt.json"), filepath.Join(f.root, "nm.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(log) + "\n" +
		"if [ \"$1 $2 $3\" != \"axi receipt --head\" ]; then echo \"unexpected no-mistakes command: $*\" >&2; exit 90; fi\n" +
		"if [ ! -e " + shellQuote(receipt) + " ]; then echo 'unknown flag: --head' >&2; exit 1; fi\n" +
		"cat " + shellQuote(receipt) + "\n"
	if err := os.WriteFile(filepath.Join(f.root, "bin", "no-mistakes"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return receipt, log
}

func writeReceipt(t *testing.T, path, head, runStatus, reviewStatus, model string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"schema": "no-mistakes.review-receipt.v1", "repository": "github.com/acme/shop", "run_id": "01RUN" + runStatus, "head_sha": head, "run_status": runStatus,
		"review": map[string]any{"stage": "review", "status": reviewStatus, "report_sha256": strings.Repeat("ab", 32), "reviewer": map[string]any{"profile": "nm-reviewer", "model": model}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNoMistakesReceiptGatesPRMergeAtExactHead(t *testing.T) {
	f := reviewEvidenceFixture(t, "no-mistakes")
	receipt, log := installFakeNoMistakes(t, f)
	ship := publishCandidate(t, f, "PR lifecycle change", "author", 17)
	head := strings.TrimSpace(gitTest(t, f.env, ship.WorktreePath, "rev-parse", "HEAD"))

	requireRefusal(t, f, "review_receipt_unsupported", "land", "t1")
	writeReceipt(t, receipt, head, "failed", "failed", "model-r")
	requireRefusal(t, f, "review_receipt_failed", "land", "t1")
	writeReceipt(t, receipt, head, "completed", "passed", "model-a")
	requireRefusal(t, f, "review_receipt_identity_ineligible", "land", "t1")
	if got := f.mustTask(t, "t1"); got.State != store.StateDone {
		t.Fatalf("refused receipts advanced the Task: %s", got.State)
	}

	writeReceipt(t, receipt, head, "completed", "passed", "model-r")
	if code, out := runReviewIdentityCLI(t, f, "land", "t1"); code != 0 || !strings.Contains(out, "landing") {
		t.Fatalf("passing receipt refused: %d %s", code, out)
	}
	observePR(t, f, ship, 17, "OPEN", "SUCCESS", head, "")
	// The pipeline now reports a different head: the stored receipt does not
	// carry over, and nothing merges.
	writeReceipt(t, receipt, strings.Repeat("c", 40), "completed", "passed", "model-r")
	requireRefusal(t, f, "review_receipt_stale", "land", "t1", "--merge")
	writeReceipt(t, receipt, head, "completed", "passed", "model-r")
	if code, out := runReviewIdentityCLI(t, f, "land", "t1", "--merge"); code != 0 {
		t.Fatalf("aligned receipt refused: %d %s", code, out)
	}
	if _, err := os.Stat(f.ghMerge); err != nil {
		t.Fatalf("merge was not issued: %v", err)
	}
	receipts, err := f.db.ReviewReceipts(context.Background(), ship.ID)
	if err != nil || len(receipts) != 1 || receipts[0].HeadSHA != head || receipts[0].ReviewerModel != "model-r" || receipts[0].RunID != "01RUNcompleted" {
		t.Fatalf("qualifying receipt record: %#v %v", receipts, err)
	}
	if _, out := runReviewIdentityCLI(t, f, "show", "t1"); !strings.Contains(out, "review_receipts") || !strings.Contains(out, receipts[0].ReceiptSHA256) {
		t.Fatalf("show omits the receipt: %s", out)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if line != "axi receipt --head "+head {
			t.Fatalf("unexpected no-mistakes query %q", line)
		}
	}
}
