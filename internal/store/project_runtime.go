package store

import "context"

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
