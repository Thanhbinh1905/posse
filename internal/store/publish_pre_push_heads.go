package store

import (
	"context"
	"time"
)

// RecordPublishPrePushHead remembers an origin head observed immediately before
// publish pushes a Task branch. It is retry evidence, not forge verification.
func (db *DB) RecordPublishPrePushHead(ctx context.Context, taskID int64, repo, sha string) error {
	if sha == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO publish_pre_push_heads(task_id, repo, request_url, head_sha, recorded_at) VALUES (?,?, '', ?, ?)`, taskID, repo, sha, time.Now().UnixMilli())
	return err
}

// WasPublishPrePushHead reports whether this Task member's origin branch was
// observed at sha before a publish push for this request. An unbound head is
// bound to the request when it is first observed there.
func (db *DB) WasPublishPrePushHead(ctx context.Context, taskID int64, repo, requestURL, sha string) (bool, error) {
	var found, unbound bool
	err := db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM publish_pre_push_heads WHERE task_id=? AND repo=? AND request_url IN ('', ?) AND head_sha=?),
		EXISTS(SELECT 1 FROM publish_pre_push_heads WHERE task_id=? AND repo=? AND request_url='' AND head_sha=?)`, taskID, repo, requestURL, sha, taskID, repo, sha).Scan(&found, &unbound)
	if err != nil || !found || !unbound || requestURL == "" {
		return found, err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE publish_pre_push_heads SET request_url=? WHERE task_id=? AND repo=? AND request_url='' AND head_sha=?`, requestURL, taskID, repo, sha); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM publish_pre_push_heads WHERE task_id=? AND repo=? AND request_url='' AND head_sha=? AND EXISTS (SELECT 1 FROM publish_pre_push_heads WHERE task_id=? AND repo=? AND request_url=? AND head_sha=?)`, taskID, repo, sha, taskID, repo, requestURL, sha); err != nil {
		return false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM publish_pre_push_heads WHERE task_id=? AND repo=? AND request_url IN ('', ?) AND head_sha=?)`, taskID, repo, requestURL, sha).Scan(&found); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return found, nil
}
