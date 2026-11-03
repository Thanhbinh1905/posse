package store

import (
	"context"
	"time"
)

func (db *DB) ClaimNoticeCommandNotifications(ctx context.Context, projectID int64, notices []Notice, at int64) ([]Notice, error) {
	if len(notices) == 0 {
		return nil, nil
	}
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	claimed := make([]Notice, 0, len(notices))
	for _, notice := range notices {
		result, err := tx.ExecContext(ctx, `UPDATE notices SET command_notified_at=? WHERE id=? AND project_id=? AND command_notified_at=0`, at, notice.ID, projectID)
		if err != nil {
			return nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed > 0 {
			notice.CommandNotifiedAt = at
			claimed = append(claimed, notice)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(claimed) > 0 {
		return claimed, db.PersistProject(ctx, projectID)
	}
	return claimed, nil
}
