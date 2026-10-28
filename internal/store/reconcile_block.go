package store

import (
	"context"
	"database/sql"
	"errors"
)

// ReconcileBlock is why the Project watch's reconcile pass has not completed
// since Since. A zero Since means the last pass completed.
type ReconcileBlock struct {
	Since        int64
	Cause        string
	NoticeRaised bool
}

func (db *DB) ReconcileBlock(ctx context.Context, projectID int64) (ReconcileBlock, error) {
	var block ReconcileBlock
	var raised int
	err := db.QueryRowContext(ctx, `SELECT reconcile_blocked_since,reconcile_block_cause,reconcile_block_notice_raised FROM project_watch_state WHERE project_id=?`, projectID).
		Scan(&block.Since, &block.Cause, &raised)
	if errors.Is(err, sql.ErrNoRows) {
		return ReconcileBlock{}, nil
	}
	block.NoticeRaised = raised != 0
	return block, err
}

// RecordReconcileBlock keeps the first blocked time of an episode and the
// latest cause.
func (db *DB) RecordReconcileBlock(ctx context.Context, projectID int64, cause string, at int64) (ReconcileBlock, error) {
	if _, err := db.ExecContext(ctx, `INSERT INTO project_watch_state(project_id,reconcile_blocked_since,reconcile_block_cause) VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET reconcile_blocked_since=CASE WHEN reconcile_blocked_since=0 THEN excluded.reconcile_blocked_since ELSE reconcile_blocked_since END,reconcile_block_cause=excluded.reconcile_block_cause`, projectID, at, cause); err != nil {
		return ReconcileBlock{}, err
	}
	return db.ReconcileBlock(ctx, projectID)
}

// ClaimReconcileBlockNotice reports whether this caller raises the episode's
// only Notice.
func (db *DB) ClaimReconcileBlockNotice(ctx context.Context, projectID int64) (bool, error) {
	result, err := db.ExecContext(ctx, `UPDATE project_watch_state SET reconcile_block_notice_raised=1 WHERE project_id=? AND reconcile_blocked_since<>0 AND reconcile_block_notice_raised=0`, projectID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (db *DB) ReleaseReconcileBlockNotice(ctx context.Context, projectID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE project_watch_state SET reconcile_block_notice_raised=0 WHERE project_id=?`, projectID)
	return err
}

func (db *DB) ClearReconcileBlock(ctx context.Context, projectID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE project_watch_state SET reconcile_blocked_since=0,reconcile_block_cause='',reconcile_block_notice_raised=0 WHERE project_id=? AND reconcile_blocked_since<>0`, projectID)
	return err
}
