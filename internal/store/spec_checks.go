package store

import (
	"context"
	"time"
)

// SpecReadyChecked reports whether the Task's spec_ready_to_deliver check has
// completed since its pull request merged.
func (db *DB) SpecReadyChecked(ctx context.Context, taskID int64) (bool, error) {
	var found bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_spec_checks WHERE task_id=?)`, taskID).Scan(&found)
	return found, err
}

func (db *DB) MarkSpecReadyChecked(ctx context.Context, taskID int64) error {
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO task_spec_checks(task_id, checked_at) VALUES (?, ?)`, taskID, time.Now().UnixMilli())
	return err
}
