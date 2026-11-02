package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type QueuedTaskDecline struct {
	Reason    string
	CheckedAt time.Time
}

func (db *DB) RecordQueuedTaskDecline(ctx context.Context, taskID int64, reason string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO queued_task_declines(task_id, reason, checked_at) VALUES (?,?,?)
		ON CONFLICT(task_id) DO UPDATE SET reason=excluded.reason, checked_at=excluded.checked_at`, taskID, reason, time.Now().UnixMilli())
	return err
}

func (db *DB) ClearQueuedTaskDecline(ctx context.Context, taskID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM queued_task_declines WHERE task_id=?`, taskID)
	return err
}

func (db *DB) QueuedTaskDeclineReason(ctx context.Context, taskID int64) (string, error) {
	var reason string
	err := db.QueryRowContext(ctx, `SELECT reason FROM queued_task_declines WHERE task_id=?`, taskID).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return reason, err
}

func (db *DB) QueuedTaskDeclines(ctx context.Context, projectID int64) (map[int64]QueuedTaskDecline, error) {
	rows, err := db.QueryContext(ctx, `SELECT d.task_id, d.reason, d.checked_at FROM queued_task_declines d JOIN tasks t ON t.id=d.task_id WHERE t.project_id=? AND t.state='queued'`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	declines := map[int64]QueuedTaskDecline{}
	for rows.Next() {
		var taskID, checkedAt int64
		var decline QueuedTaskDecline
		if err := rows.Scan(&taskID, &decline.Reason, &checkedAt); err != nil {
			return nil, err
		}
		decline.CheckedAt = time.UnixMilli(checkedAt)
		declines[taskID] = decline
	}
	return declines, rows.Err()
}
