package store

import (
	"context"
	"encoding/json"
	"time"
)

// WatchRuntime is the durable identity and liveness record for a Project's
// detached PR/CI watch.
type WatchRuntime struct {
	PID              int
	Token            string
	StartedAt        int64
	HeartbeatAt      int64
	ExecutablePath   string
	ExecutableDevice uint64
	ExecutableInode  uint64
}

func (db *DB) WatchRuntime(ctx context.Context, projectID int64) (WatchRuntime, error) {
	var watch WatchRuntime
	var device, inode int64
	err := db.QueryRowContext(ctx, `SELECT pid,token,started_at,heartbeat_at,executable_path,executable_device,executable_inode FROM project_lookout_pollers WHERE project_id=?`, projectID).
		Scan(&watch.PID, &watch.Token, &watch.StartedAt, &watch.HeartbeatAt, &watch.ExecutablePath, &device, &inode)
	if IsNotFound(err) {
		return WatchRuntime{}, nil
	}
	watch.ExecutableDevice, watch.ExecutableInode = uint64(device), uint64(inode)
	return watch, err
}

func (db *DB) SetWatchRuntime(ctx context.Context, projectID int64, watch WatchRuntime) error {
	_, err := db.ExecContext(ctx, `INSERT INTO project_lookout_pollers(project_id,pid,token,started_at,heartbeat_at,executable_path,executable_device,executable_inode,failure_notice_raised) VALUES(?,?,?,?,?,?,?,?,0)
		ON CONFLICT(project_id) DO UPDATE SET pid=excluded.pid,token=excluded.token,started_at=excluded.started_at,heartbeat_at=excluded.heartbeat_at,executable_path=excluded.executable_path,executable_device=excluded.executable_device,executable_inode=excluded.executable_inode,failure_notice_raised=0`,
		projectID, watch.PID, watch.Token, watch.StartedAt, watch.HeartbeatAt, watch.ExecutablePath, int64(watch.ExecutableDevice), int64(watch.ExecutableInode))
	return err
}

func (db *DB) HeartbeatWatchRuntime(ctx context.Context, projectID int64, pid int, token string, at int64) (bool, error) {
	result, err := db.ExecContext(ctx, `UPDATE project_lookout_pollers SET heartbeat_at=?,failure_notice_raised=0 WHERE project_id=? AND pid=? AND token=?`, at, projectID, pid, token)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func (db *DB) ClearWatchRuntime(ctx context.Context, projectID int64, pid int, token string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM project_lookout_pollers WHERE project_id=? AND pid=? AND token=?`, projectID, pid, token)
	return err
}

// RaiseWatchRuntimeFailure creates one Notice for a stale-watch episode.
// The row update and Notice insert share a transaction, so concurrent commands
// cannot duplicate the report or mark it raised without recording it.
func (db *DB) RaiseWatchRuntimeFailure(ctx context.Context, projectID int64, token, summary string, at int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE project_lookout_pollers SET failure_notice_raised=1 WHERE project_id=? AND token=? AND failure_notice_raised=0`, projectID, token)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return false, err
	}
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	dataJSON, err := json.Marshal(map[string]string{"watch_episode": token})
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,kind,summary,data_json,created_at) VALUES(?,'pr_watch_failing',?,?,?)`, projectID, summary, string(dataJSON), at); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if err := db.PersistProject(ctx, projectID); err != nil {
		return true, err
	}
	return true, nil
}
