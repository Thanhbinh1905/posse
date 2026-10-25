package store

import (
	"strings"
	"testing"
)

func TestStoredReviewReceiptsStayImmutableAndSurviveRebuild(t *testing.T) {
	f := newReviewVerdictFixture(t, nil)
	receipt := ReviewReceipt{
		TaskID: f.change, Repository: "github.com/acme/shop", RunID: "01RUN", HeadSHA: reviewShaA, ReviewStatus: "passed",
		ReviewerProfile: "nm-reviewer", ReviewerModel: "model-r", ReportSHA256: reportHash, ReceiptSHA256: strings.Repeat("d", 64), ObservedAt: 10,
	}
	// Releases before the no-mistakes removal wrote receipts; this one only reads them.
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO review_receipts(task_id,repository,run_id,head_sha,review_status,reviewer_profile,reviewer_model,report_sha256,receipt_sha256,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		receipt.TaskID, receipt.Repository, receipt.RunID, receipt.HeadSHA, receipt.ReviewStatus, receipt.ReviewerProfile, receipt.ReviewerModel, receipt.ReportSHA256, receipt.ReceiptSHA256, receipt.ObservedAt); err != nil {
		t.Fatal(err)
	}
	if err := f.db.PersistTask(f.ctx, f.change); err != nil {
		t.Fatal(err)
	}
	if receipts, err := f.db.ReviewReceipts(f.ctx, f.change); err != nil || len(receipts) != 1 || receipts[0] != receipt {
		t.Fatalf("receipts = %#v, %v", receipts, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_receipts SET head_sha=? WHERE task_id=?`, reviewShaB, f.change); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("receipt update was not refused: %v", err)
	}
	if _, err := f.db.RebuildFromSnapshots(f.ctx, f.home); err != nil {
		t.Fatal(err)
	}
	if receipts, err := f.db.ReviewReceipts(f.ctx, f.change); err != nil || len(receipts) != 1 || receipts[0] != receipt {
		t.Fatalf("rebuilt receipts = %#v, %v", receipts, err)
	}
}
