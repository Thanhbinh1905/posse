package store

import "context"

// LookoutRecovery stores retry state independently of the short-lived Lead
// watcher process, so a Notice-triggered re-arm cannot reset its backoff.
type LookoutRecovery struct {
	PaneID       string
	RetryAt      int64
	FailedStarts int
	NoticeRaised bool
}

func (db *DB) LookoutRecovery(ctx context.Context, projectID int64) (LookoutRecovery, error) {
	var state LookoutRecovery
	var noticeRaised int
	err := db.QueryRowContext(ctx, `SELECT lookout_pane_id,lookout_retry_at,lookout_failed_starts,lookout_notice_raised FROM project_runtime WHERE project_id=?`, projectID).
		Scan(&state.PaneID, &state.RetryAt, &state.FailedStarts, &noticeRaised)
	if IsNotFound(err) {
		return LookoutRecovery{}, nil
	}
	state.NoticeRaised = noticeRaised != 0
	return state, err
}

func (db *DB) SetLookoutRecovery(ctx context.Context, projectID int64, state LookoutRecovery) error {
	noticeRaised := 0
	if state.NoticeRaised {
		noticeRaised = 1
	}
	_, err := db.ExecContext(ctx, `INSERT INTO project_runtime(project_id,server_started_at,lookout_pane_id,lookout_retry_at,lookout_failed_starts,lookout_notice_raised) VALUES(?,'',?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET lookout_pane_id=excluded.lookout_pane_id,lookout_retry_at=excluded.lookout_retry_at,lookout_failed_starts=excluded.lookout_failed_starts,lookout_notice_raised=excluded.lookout_notice_raised`, projectID, state.PaneID, state.RetryAt, state.FailedStarts, noticeRaised)
	return err
}

func (db *DB) ResetLookoutRecovery(ctx context.Context, projectID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE project_runtime SET lookout_pane_id='',lookout_retry_at=0,lookout_failed_starts=0,lookout_notice_raised=0 WHERE project_id=?`, projectID)
	return err
}

func (db *DB) ProjectServerStartedAt(ctx context.Context, projectID int64) (string, error) {
	var startedAt string
	err := db.QueryRowContext(ctx, `SELECT server_started_at FROM project_runtime WHERE project_id=?`, projectID).Scan(&startedAt)
	if IsNotFound(err) {
		return "", nil
	}
	return startedAt, err
}

func (db *DB) RememberProjectServerStartedAt(ctx context.Context, projectID int64, startedAt string) error {
	if startedAt == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO project_runtime(project_id,server_started_at) VALUES(?,?)`, projectID, startedAt)
	return err
}

func (db *DB) SetProjectServerStartedAt(ctx context.Context, projectID int64, startedAt string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO project_runtime(project_id,server_started_at) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET server_started_at=excluded.server_started_at`, projectID, startedAt)
	return err
}
