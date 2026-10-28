//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// merge_ready tells the Lead to merge, so it must not be raised while the
// review gate of `posse merge` refuses the green head (#615).
func TestChangesRequestedReviewRoundWithGreenChecksRaisesNoMergeReady(t *testing.T) {
	f := reviewEvidenceFixture(t, "quick")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.enabled", "true")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "review_loop.auto_start", "false")
	change := publishCandidate(t, f, "Reviewed PR change", "author", 17)
	if code, output := runReviewIdentityCLI(t, f, "review", "start", "t1", "--profile", "reviewer"); code != 0 {
		t.Fatalf("start Review Round: %d %s", code, output)
	}
	review := f.mustTask(t, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	report := filepath.Join(f.home, "scratch", "shop", "t2", "report.md")
	findings := filepath.Join(f.home, "scratch", "shop", "t2", "findings.json")
	if err := os.WriteFile(report, []byte("The candidate contains one blocking pattern.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(findings, []byte(blockingFindings("unsafe-candidate", "candidate-t1.txt", "candidate Reviewed PR change")), 0o600); err != nil {
		t.Fatal(err)
	}
	signalReviewPolicyTask(t, f, review, "--report", report, "--findings", findings, "--verdict", "changes_requested")
	head := strings.TrimSpace(gitTest(t, f.env, change.WorktreePath, "rev-parse", "HEAD"))
	observePR(t, f, change, 17, "OPEN", "SUCCESS", head, "")
	requireRefusal(t, f, "review_required", "merge", "t1")
	var count int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='merge_ready'`, change.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("merge_ready raised %d time(s) for a green PR whose Review Round requested changes", count)
	}
}
