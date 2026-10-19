package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestQueueCIFixInstructionEnforcesBudgetAtomically(t *testing.T) {
	fixture := newReviewVerdictFixture(t, nil)
	if err := fixture.db.Transition(fixture.ctx, fixture.ship, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(fixture.ctx, fixture.ship, StateWorking, StateDone, "worker", "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(fixture.ctx, `UPDATE tasks SET landing_mode='pr',gated_sha=?,pr_url='https://github.com/acme/shop/pull/1' WHERE id=?`, reviewShaA, fixture.ship); err != nil {
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
			_, err := fixture.db.QueueCIFixInstruction(context.Background(), fixture.ship, reviewShaA, "https://github.com/acme/shop/pull/1", "ci:"+check, "Fix "+check, "", 1)
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
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, fixture.ship).Scan(&rounds); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRowContext(fixture.ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND status='queued'`, fixture.ship).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if rounds != 1 || messages != 1 {
		t.Fatalf("rounds=%d queued messages=%d, want one each", rounds, messages)
	}
}
