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

func TestGreenhouseReviewRequestLedgerIsImmutableAndRecordsEveryOutcome(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	requests := []GreenhouseReviewRequest{
		{ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: reviewShaA, Provider: GreenhouseReviewProvider, RequestedAt: 100, SettledAt: 120, Outcome: "failed"},
		{ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: reviewShaA, Provider: GreenhouseReviewProvider, RequestedAt: 130, SettledAt: 150, Outcome: "head_mismatch"},
	}
	for _, request := range requests {
		if err := f.db.RecordGreenhouseReviewRequest(f.ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.db.GreenhouseReviewRequests(f.ctx, task.ProjectID)
	if err != nil || len(got) != 2 || got[0].Outcome != "failed" || got[1].Outcome != "head_mismatch" {
		t.Fatalf("request ledger = %+v, %v", got, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE greenhouse_review_requests SET outcome='verdict' WHERE id=?`, got[0].ID); err == nil {
		t.Fatal("updated an insert-only request row")
	}
}

func TestGreenhouseReviewLedgerKeepsResumeOutcomeForSameGrant(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	grant := "shop/t1/shop/r1"
	failed := GreenhouseReviewRequest{ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: reviewShaA, Provider: GreenhouseReviewProvider, RequestedAt: 100, SettledAt: 200, Outcome: "failed", Grant: grant}
	if err := f.db.RecordGreenhouseReviewRequest(f.ctx, failed); err != nil {
		t.Fatal(err)
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouseVerdict(task.ID, grant, reviewShaA), func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record resumed Verdict = %v, %v", recorded, err)
	}
	settled := failed
	settled.SettledAt, settled.Outcome = 310, "verdict"
	if err := f.db.RecordGreenhouseReviewRequest(f.ctx, settled); err != nil {
		t.Fatal(err)
	}
	requests, err := f.db.GreenhouseReviewRequests(f.ctx, task.ProjectID)
	if err != nil || len(requests) != 1 || requests[0].Outcome != "verdict" || requests[0].VerdictID == nil || requests[0].RequestedAt != failed.RequestedAt {
		t.Fatalf("resumed request rows = %+v, %v; want one request with its final outcome", requests, err)
	}
}

func TestGreenhouseFailureNoticeOncePerHead(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	for _, reason := range []string{"network failure", "protocol refusal"} {
		if _, err := f.db.CreateGreenhouseFailureNoticeOnce(f.ctx, task.ProjectID, task.ID, "shop", reviewShaA, reason); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='greenhouse_review_failed'`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failure Notices = %d, %v; want one per head", count, err)
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
