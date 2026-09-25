package store

import (
	"context"
	"database/sql"
)

type ProjectRecovery struct {
	Generation string
	OwnerPID   int
	ClaimedAt  int64
}

func (db *DB) ProjectRecovery(ctx context.Context, projectID int64) (ProjectRecovery, error) {
	var state ProjectRecovery
	err := db.QueryRowContext(ctx, `SELECT recovery_generation,recovery_owner_pid,recovery_claimed_at FROM project_runtime WHERE project_id=?`, projectID).
		Scan(&state.Generation, &state.OwnerPID, &state.ClaimedAt)
	if IsNotFound(err) {
		return ProjectRecovery{}, nil
	}
	return state, err
}

func (db *DB) ClaimProjectRecovery(ctx context.Context, projectID int64, generation string, ownerPID int, now, staleBefore int64) (bool, ProjectRecovery, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, ProjectRecovery{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_runtime(project_id,server_started_at) VALUES(?, '')`, projectID); err != nil {
		return false, ProjectRecovery{}, err
	}
	var previous ProjectRecovery
	if err := tx.QueryRowContext(ctx, `SELECT recovery_generation,recovery_owner_pid,recovery_claimed_at FROM project_runtime WHERE project_id=?`, projectID).
		Scan(&previous.Generation, &previous.OwnerPID, &previous.ClaimedAt); err != nil {
		return false, ProjectRecovery{}, err
	}
	if previous.Generation == generation && previous.OwnerPID == 0 {
		return false, previous, nil
	}
	if previous.OwnerPID != 0 && previous.ClaimedAt >= staleBefore {
		return false, previous, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE project_runtime SET recovery_generation=?,recovery_owner_pid=?,recovery_claimed_at=? WHERE project_id=? AND recovery_generation=? AND recovery_owner_pid=? AND recovery_claimed_at=?`, generation, ownerPID, now, projectID, previous.Generation, previous.OwnerPID, previous.ClaimedAt)
	if err != nil {
		return false, previous, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return false, previous, err
	}
	if err := tx.Commit(); err != nil {
		return false, previous, err
	}
	return true, previous, nil
}

func (db *DB) FinishProjectRecovery(ctx context.Context, projectID int64, generation string, ownerPID int, previousGeneration string, complete bool) error {
	claimedGeneration := generation
	if !complete {
		claimedGeneration = previousGeneration
	}
	result, err := db.ExecContext(ctx, `UPDATE project_runtime SET recovery_generation=?,recovery_owner_pid=0,recovery_claimed_at=0 WHERE project_id=? AND recovery_generation=? AND recovery_owner_pid=?`, claimedGeneration, projectID, generation, ownerPID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 1 {
		return err
	}
	return sql.ErrNoRows
}
