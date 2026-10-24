package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestReviewAndCIFixesShareTheFrozenReviewCap(t *testing.T) {
	fixture := newReviewVerdictFixture(t, nil)
	if err := fixture.db.Transition(fixture.ctx, fixture.change, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(fixture.ctx, fixture.change, StateWorking, StateDone, "worker", "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(fixture.ctx, `UPDATE tasks SET landing_mode='pr',gated_sha=?,pr_url='https://github.com/acme/shop/pull/1' WHERE id=?`, reviewShaA, fixture.change); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(fixture.ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','verdict','changes_requested',1)`, fixture.change, fixture.review, reviewShaA, reviewShaA, reviewShaA, reviewShaA, reportHash); err != nil {
		t.Fatal(err)
	}
	review, err := fixture.db.QueueValidationFixInstructionWithSharedBudget(fixture.ctx, fixture.change, "review", reviewShaA, "review:shared", "Fix review findings", 3, 2)
	if err != nil || !review.Queued {
		t.Fatalf("queue Review repair: %+v %v", review, err)
	}
	ci, err := fixture.db.QueueCIFixInstructionWithSharedBudget(fixture.ctx, fixture.change, reviewShaA, "https://github.com/acme/shop/pull/1", "ci:shared", "Fix CI", "", 3, 2)
	if err != nil || !ci.SharedExhausted || ci.Queued {
		t.Fatalf("CI repair did not hit shared cap: %+v %v", ci, err)
	}
	usage, err := fixture.db.ReviewCapUsage(fixture.ctx, fixture.change)
	if err != nil || usage.Total() != 2 || usage.RepairRounds != 1 || usage.ReviewRounds != 1 {
		t.Fatalf("shared cap accounting=%+v err=%v", usage, err)
	}
}

func TestActiveReviewRoundReservesSharedRepairCapacity(t *testing.T) {
	fixture := newReviewVerdictFixture(t, nil)
	if _, err := fixture.db.ExecContext(fixture.ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','admitted',NULL,1)`, fixture.change, fixture.review, reviewShaA, reviewShaA, reviewShaA, reviewShaA, reportHash); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.db.QueueValidationFixInstructionWithSharedBudget(fixture.ctx, fixture.change, "review", reviewShaA, "review:reserved", "Fix review findings", 3, 1)
	if err != nil || !result.SharedExhausted || result.Queued {
		t.Fatalf("active Review did not reserve shared capacity: %+v %v", result, err)
	}
}

func TestQueueCIFixInstructionEnforcesBudgetAtomically(t *testing.T) {
	fixture := newReviewVerdictFixture(t, nil)
	if err := fixture.db.Transition(fixture.ctx, fixture.change, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(fixture.ctx, fixture.change, StateWorking, StateDone, "worker", "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(fixture.ctx, `UPDATE tasks SET landing_mode='pr',gated_sha=?,pr_url='https://github.com/acme/shop/pull/1' WHERE id=?`, reviewShaA, fixture.change); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, check := range []string{"unit", "integration"} {
		check := check
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := fixture.db.QueueCIFixInstruction(context.Background(), fixture.change, reviewShaA, "https://github.com/acme/shop/pull/1", "ci:"+check, "Fix "+check, "", 1)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	queued, exhausted := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			queued++
		case errors.Is(err, ErrValidationFixBudgetExhausted):
			exhausted++
		default:
			t.Fatalf("queue CI fix error = %v", err)
		}
	}
	if queued != 1 || exhausted != 1 {
		t.Fatalf("queued=%d exhausted=%d, want one each", queued, exhausted)
	}
	var rounds, messages int
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, fixture.change).Scan(&rounds); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.change).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if rounds != 1 || messages != 1 {
		t.Fatalf("rounds=%d queued messages=%d, want one each", rounds, messages)
	}
}

func TestCIFixQueuedNoticeSurvivesDeliveryReopeningTheTask(t *testing.T) {
	fixture := newReviewVerdictFixture(t, nil)
	prURL := "https://github.com/acme/shop/pull/1"
	if err := fixture.db.Transition(fixture.ctx, fixture.change, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(fixture.ctx, fixture.change, StateWorking, StateDone, "worker", "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(fixture.ctx, `UPDATE tasks SET landing_mode='pr',gated_sha=?,pr_url=? WHERE id=?`, reviewShaA, prURL, fixture.change); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.db.QueueCIFixInstruction(fixture.ctx, fixture.change, reviewShaA, prURL, "ci:unit", "Fix unit", "", 2)
	if err != nil || !result.Queued {
		t.Fatalf("queue CI fix: %#v err=%v", result, err)
	}
	// Delivering the instruction reopens the Task and clears the gated head
	// before the caller records the Notice.
	if err := fixture.db.Transition(fixture.ctx, fixture.change, StateDone, StateWorking, "lead", "Lead submitted an instruction"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(fixture.ctx, `UPDATE tasks SET gated_sha='' WHERE id=?`, fixture.change); err != nil {
		t.Fatal(err)
	}
	var projectID int64
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT project_id FROM tasks WHERE id=?`, fixture.change).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	notice := Notice{ProjectID: projectID, TaskID: fixture.change, Kind: "ci_fix_queued", Summary: "queued", DataJSON: `{"head_sha":"` + reviewShaA + `","check_set":"set"}`}
	inserted, err := fixture.db.CreateValidationNoticeOnce(fixture.ctx, notice, prURL, reviewShaA, "set")
	if err != nil || !inserted {
		t.Fatalf("ci_fix_queued Notice after delivery: inserted=%v err=%v", inserted, err)
	}
	if inserted, err = fixture.db.CreateValidationNoticeOnce(fixture.ctx, notice, prURL, reviewShaA, "set"); err != nil || inserted {
		t.Fatalf("duplicate ci_fix_queued Notice: inserted=%v err=%v", inserted, err)
	}
}
