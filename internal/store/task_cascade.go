package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CascadeTeardownQueued tears down queued Tasks and retires unanswered Decisions atomically, returning IDs that raced.
func (db *DB) CascadeTeardownQueued(ctx context.Context, tasks []Task, note string) ([]int64, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	queries := db.queries.WithTx(tx.Tx)
	dependencies := make(map[int64][]string, len(tasks))
	refs := make(map[int64]string, len(tasks))
	for _, task := range tasks {
		refs[task.ID] = fmt.Sprintf("t%d", task.Seq)
		plan, err := task.AdmittedPlan()
		if err != nil {
			return nil, err
		}
		if plan != nil {
			for _, dependency := range plan.Dependencies {
				dependencies[task.ID] = append(dependencies[task.ID], dependency.TaskID)
			}
		}
	}
	var raced, changed []int64
	blockedRefs := make(map[string]bool)
	for _, task := range tasks {
		blocked := false
		for _, dependency := range dependencies[task.ID] {
			if blockedRefs[dependency] {
				blocked = true
				break
			}
		}
		if blocked {
			blockedRefs[refs[task.ID]] = true
			continue
		}
		var state, planJSON string
		var planRevision int
		if err := tx.QueryRowContext(ctx, `SELECT state,plan_json,plan_revision FROM tasks WHERE id=?`, task.ID).Scan(&state, &planJSON, &planRevision); err != nil {
			return nil, err
		}
		if state != string(StateQueued) || planJSON != task.PlanJSON || planRevision != task.PlanRevision {
			raced = append(raced, task.ID)
			blockedRefs[refs[task.ID]] = true
			continue
		}
		if err := transitionTx(ctx, queries, task.ID, StateQueued, StateTornDown, "cli", note); err != nil {
			if errors.Is(err, ErrStateRace) {
				raced = append(raced, task.ID)
				blockedRefs[refs[task.ID]] = true
				continue
			}
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason='Task was torn down' WHERE task_id=? AND kind<>'orphan' AND answered_at=0 AND obsolete_at=0`, time.Now().UnixMilli(), task.ID); err != nil {
			return nil, err
		}
		changed = append(changed, task.ID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, taskID := range changed {
		if err := db.PersistTask(ctx, taskID); err != nil {
			return nil, err
		}
	}
	return raced, nil
}
