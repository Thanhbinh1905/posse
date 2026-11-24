package store

import (
	"context"
	"errors"
	"time"
)

// CascadeTeardownQueued tears down queued Tasks and retires unanswered Decisions atomically, returning IDs that raced.
func (db *DB) CascadeTeardownQueued(ctx context.Context, taskIDs []int64, note string) ([]int64, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	queries := db.queries.WithTx(tx.Tx)
	var raced, changed []int64
	for _, taskID := range taskIDs {
		if err := transitionTx(ctx, queries, taskID, StateQueued, StateTornDown, "cli", note); err != nil {
			if errors.Is(err, ErrStateRace) {
				raced = append(raced, taskID)
				continue
			}
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason='Task was torn down' WHERE task_id=? AND kind<>'orphan' AND answered_at=0 AND obsolete_at=0`, time.Now().UnixMilli(), taskID); err != nil {
			return nil, err
		}
		changed = append(changed, taskID)
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
