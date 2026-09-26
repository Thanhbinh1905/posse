package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// RestoreMergedTaskWithWork keeps the Rider reportable when a merged PR has
// work that cannot be released. State, landed ref and Notice change together.
func (db *DB) RestoreMergedTaskWithWork(ctx context.Context, projectID int64, task Task) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state State
	var url string
	if err := tx.QueryRowContext(ctx, `SELECT state, pr_url FROM tasks WHERE id=? AND project_id=?`, task.ID, projectID).Scan(&state, &url); err != nil {
		return err
	}
	if state != StateLanded || url != task.PRURL {
		return fmt.Errorf("%w: merged Task changed before follow-up recovery", ErrStateRace)
	}
	if err := transitionTx(ctx, db.queries.WithTx(tx), task.ID, StateLanded, StateWorking, "cli", "Unmerged Task work remains after PR merge"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET landed_ref='' WHERE id=?`, task.ID); err != nil {
		return err
	}
	var found bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND task_id=? AND kind='pr_follow_up_pending')`, projectID, task.ID).Scan(&found); err != nil {
		return err
	}
	if !found {
		data, err := json.Marshal(map[string]string{"url": task.PRURL})
		if err != nil {
			return err
		}
		if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, TaskID: task.ID, Kind: "pr_follow_up_pending", Summary: fmt.Sprintf("%s: unmerged follow-up work remains; retain the Rider or run `posse relaunch t%d` if its pane closed", task.Title, task.Seq), DataJSON: string(data)}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, task.ID)
}
