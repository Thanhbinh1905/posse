//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// startSecondReviewRound passes Round 1 of a quick-loop Change Task, commits
// a follow-up that leaves candidate.txt out of the Round 2 delta, and starts
// Round 2. It returns the Change Task and the Round 2 Review Task.
func startSecondReviewRound(t *testing.T, f *prLifecycleFixture) (store.Task, store.Task) {
	t.Helper()
	change := startQuickLoopChange(t, f, "false")
	recordPassingReview(t, f, "t1")
	commitFile(t, f, change, "follow-up.txt", "follow-up change")
	resubmitChange(t, f, change, "Follow-up for Round 2")
	if code, output := runReviewIdentityCLI(t, f, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round 2: %d %s", code, output)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	review := tasks[len(tasks)-1]
	waitForReviewIdentityAgent(t, f, review.PaneID)
	return change, review
}

// signalReviewRound completes a Review Round with findings and returns the
// exit code and output, so refused verdicts can be inspected.
func signalReviewRound(t *testing.T, f *prLifecycleFixture, review store.Task, verdict, findings string) (int, string) {
	t.Helper()
	scratch := filepath.Join(f.home, "scratch", "shop", taskIDOf(review))
	report, findingsPath := filepath.Join(scratch, "report.md"), filepath.Join(scratch, "findings.json")
	if err := os.WriteFile(report, []byte("Review Round recorded "+verdict+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findingsPath, []byte(findings), 0o600); err != nil {
		t.Fatal(err)
	}
	return signalReviewPolicyTaskAllowFailure(t, f, review, "--report", report, "--findings", findingsPath, "--verdict", verdict)
}

func reviewRoundVerdict(t *testing.T, f *prLifecycleFixture, review store.Task) string {
	t.Helper()
	rounds, err := f.db.ReviewRoundForTask(context.Background(), review.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("Review Round bindings: %#v %v", rounds, err)
	}
	return rounds[0].Verdict
}

// Reproduces t552 and t575: a Round 2 P1 on a line the Change adds in an
// earlier round lies outside the delta but must still block.
func TestIntroducedP1OnChangeAddedLineOutsideDeltaRequestsChanges(t *testing.T) {
	f := reviewIdentityFixture(t)
	change, review := startSecondReviewRound(t, f)
	code, output := signalReviewRound(t, f, review, "changes_requested", blockingFindings("unsafe-candidate", "candidate.txt", "candidate change"))
	if code != 0 {
		t.Fatalf("introduced P1 on a Change-added line was refused: %d %s", code, output)
	}
	if verdict := reviewRoundVerdict(t, f, review); verdict != store.ReviewChangesRequested {
		t.Fatalf("Review Round verdict=%q, want changes_requested", verdict)
	}
	var origin string
	var inScope int
	if err := f.db.QueryRowContext(context.Background(), `SELECT origin,in_scope FROM review_findings WHERE ship_task_id=? AND path='candidate.txt'`, change.ID).Scan(&origin, &inScope); err != nil {
		t.Fatal(err)
	}
	if origin != "introduced" || inScope != 1 {
		t.Fatalf("finding recorded origin=%s in_scope=%d, want introduced in scope", origin, inScope)
	}
}

func TestOriginReclassificationIsReportedInsteadOfSilentlyPassing(t *testing.T) {
	f := reviewIdentityFixture(t)
	change, review := startSecondReviewRound(t, f)
	findings := blockingFindings("unsafe-readme", "README.md", "fixture")
	code, output := signalReviewRound(t, f, review, "changes_requested", findings)
	if code == 0 || !strings.Contains(output, "review_verdict_inconsistent") || !strings.Contains(output, "README.md:1") || !strings.Contains(output, "pre_existing") {
		t.Fatalf("refusal did not name the reclassified finding: %d %s", code, output)
	}
	if code, output := signalReviewRound(t, f, review, "inconclusive", findings); code != 0 {
		t.Fatalf("inconclusive Review Round was refused: %d %s", code, output)
	}
	var summary, data string
	if err := f.db.QueryRowContext(context.Background(), `SELECT summary,data_json FROM notices WHERE task_id=? AND kind='review_round_done' ORDER BY id DESC LIMIT 1`, change.ID).Scan(&summary, &data); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		OriginReclassified []string `json:"origin_reclassified"`
	}
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.OriginReclassified) != 1 || !strings.Contains(decoded.OriginReclassified[0], "README.md:1") || !strings.Contains(summary, "recorded pre_existing") {
		t.Fatalf("review_round_done hid the origin reclassification: summary=%q data=%s", summary, data)
	}
}
