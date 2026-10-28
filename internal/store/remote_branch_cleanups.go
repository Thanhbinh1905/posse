package store

import (
	"context"
	"time"
)

// RemoteBranchCleanup is a merged Task branch whose remote deletion failed.
type RemoteBranchCleanup struct {
	ProjectID int64
	TaskID    int64
	Repo      string
	Branch    string
	HeadSHA   string
	Cause     string
	Attempts  int
	FailedAt  int64
}

// RecordRemoteBranchCleanupFailure keeps the cause of a failed remote branch
// deletion and counts the attempts.
func (db *DB) RecordRemoteBranchCleanupFailure(ctx context.Context, cleanup RemoteBranchCleanup) error {
	_, err := db.ExecContext(ctx, `INSERT INTO remote_branch_cleanups(task_id, repo, project_id, branch, head_sha, cause, attempts, failed_at) VALUES (?,?,?,?,?,?,1,?)
		ON CONFLICT(task_id, repo) DO UPDATE SET branch=excluded.branch, head_sha=excluded.head_sha, cause=excluded.cause, attempts=attempts+1, failed_at=excluded.failed_at`,
		cleanup.TaskID, cleanup.Repo, cleanup.ProjectID, cleanup.Branch, cleanup.HeadSHA, cleanup.Cause, time.Now().UnixMilli())
	return err
}

// ClearRemoteBranchCleanup forgets a pending deletion once it is settled.
func (db *DB) ClearRemoteBranchCleanup(ctx context.Context, taskID int64, repo string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM remote_branch_cleanups WHERE task_id=? AND repo=?`, taskID, repo)
	return err
}

// RemoteBranchCleanups lists the pending remote branch deletions of a Project.
func (db *DB) RemoteBranchCleanups(ctx context.Context, projectID int64) ([]RemoteBranchCleanup, error) {
	rows, err := db.QueryContext(ctx, `SELECT project_id, task_id, repo, branch, head_sha, cause, attempts, failed_at FROM remote_branch_cleanups WHERE project_id=? ORDER BY task_id, repo`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cleanups []RemoteBranchCleanup
	for rows.Next() {
		var cleanup RemoteBranchCleanup
		if err := rows.Scan(&cleanup.ProjectID, &cleanup.TaskID, &cleanup.Repo, &cleanup.Branch, &cleanup.HeadSHA, &cleanup.Cause, &cleanup.Attempts, &cleanup.FailedAt); err != nil {
			return nil, err
		}
		cleanups = append(cleanups, cleanup)
	}
	return cleanups, rows.Err()
}
