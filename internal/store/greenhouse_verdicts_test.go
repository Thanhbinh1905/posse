package store

import (
	"context"
	"errors"
	"testing"
)

func greenhouseVerdict(taskID int64, grant, head string) Verdict {
	return Verdict{TaskID: taskID, Repository: "shop", Provider: GreenhouseReviewProvider, Protocol: "2", Grant: grant,
		Head: head, Target: reviewShaA, ReviewBase: reviewShaA, Outcome: "clean", Mode: "parallel"}
}

func TestRecordGreenhouseVerdictChecksHeadInsideTheInsert(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	headNow := func(head string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return head, nil }
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouseVerdict(f.change, "shop/t1/shop/r1", reviewShaB), headNow(reviewShaA)); !errors.Is(err, ErrVerdictHeadMoved) || recorded {
		t.Fatalf("a moved head recorded = %v, err = %v; want ErrVerdictHeadMoved", recorded, err)
	}
	if verdicts, err := f.db.VerdictsFor(f.ctx, f.change); err != nil || len(verdicts) != 0 {
		t.Fatalf("Verdicts after a refused insert = %+v, %v", verdicts, err)
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouseVerdict(f.change, "shop/t1/shop/r2", reviewShaB), headNow(reviewShaB)); err != nil || !recorded {
		t.Fatalf("an unmoved head recorded = %v, err = %v", recorded, err)
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouseVerdict(f.change, "shop/t1/shop/r2", reviewShaB), headNow(reviewShaB)); err != nil || recorded {
		t.Fatalf("a second Verdict for grant r2 recorded = %v, err = %v", recorded, err)
	}
	verdicts, err := f.db.VerdictsFor(f.ctx, f.change)
	if err != nil || len(verdicts) != 1 || verdicts[0].Provider != GreenhouseReviewProvider || verdicts[0].Grant != "shop/t1/shop/r2" || verdicts[0].Qualifies(reviewShaB) {
		t.Fatalf("Verdicts = %+v, %v; want one parallel Greenhouse Verdict that no gate accepts", verdicts, err)
	}
}

func TestGreenhouseRecoveryNoticeOncePerRecovery(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	for _, recovery := range []string{"rec-1", "rec-1", "rec-2"} {
		if _, err := f.db.CreateGreenhouseRecoveryNoticeOnce(f.ctx, task.ProjectID, task.ID, "shop", recovery, "parked"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='greenhouse_recovery_required'`, task.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("recovery Notices = %d, %v; want one per recovery", count, err)
	}
}
