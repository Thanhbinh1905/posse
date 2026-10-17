package store

import (
	"strings"
	"testing"
)

func TestReviewReceiptsAreInsertOnceImmutableAndSurviveRebuild(t *testing.T) {
	f := newReviewVerdictFixture(t, nil)
	receipt := ReviewReceipt{
		TaskID: f.ship, Repository: "github.com/acme/shop", RunID: "01RUN", HeadSHA: reviewShaA, ReviewStatus: "passed",
		ReviewerProfile: "nm-reviewer", ReviewerModel: "model-r", ReportSHA256: reportHash, ReceiptSHA256: strings.Repeat("d", 64), ObservedAt: 10,
	}
	for range 2 {
		if err := f.db.RecordReviewReceipt(f.ctx, receipt); err != nil {
			t.Fatal(err)
		}
	}
	later := receipt
	later.ObservedAt = 20
	if err := f.db.RecordReviewReceipt(f.ctx, later); err != nil {
		t.Fatal(err)
	}
	if receipts, err := f.db.ReviewReceipts(f.ctx, f.ship); err != nil || len(receipts) != 1 || receipts[0] != receipt {
		t.Fatalf("receipts = %#v, %v", receipts, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_receipts SET head_sha=? WHERE task_id=?`, reviewShaB, f.ship); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("receipt update was not refused: %v", err)
	}
	if _, err := f.db.RebuildFromSnapshots(f.ctx, f.home); err != nil {
		t.Fatal(err)
	}
	if receipts, err := f.db.ReviewReceipts(f.ctx, f.ship); err != nil || len(receipts) != 1 || receipts[0] != receipt {
		t.Fatalf("rebuilt receipts = %#v, %v", receipts, err)
	}
}
