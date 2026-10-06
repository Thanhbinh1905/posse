package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// TaskRecovery is a durable retry episode. A new Herdr generation cannot reset
// an unfinished episode's budget; only a successful or explicit relaunch can.
type TaskRecovery struct {
	Generation    string `json:"generation"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt int64  `json:"next_attempt_at"`
	OwnerPID      int    `json:"owner_pid"`
	Status        string `json:"status"`
	LastError     string `json:"last_error"`
}

func (db *DB) TaskRecovery(ctx context.Context, taskID int64) (TaskRecovery, error) {
	var state TaskRecovery
	err := db.QueryRowContext(ctx, `SELECT generation,attempts,next_attempt_at,owner_pid,status,last_error FROM task_recovery WHERE task_id=?`, taskID).Scan(&state.Generation, &state.Attempts, &state.NextAttemptAt, &state.OwnerPID, &state.Status, &state.LastError)
	if IsNotFound(err) {
		return TaskRecovery{}, nil
	}
	return state, err
}

// ClaimTaskRecovery charges an attempt before any process can be started. The
// expected owner is checked atomically, including takeover of a dead owner.
func (db *DB) ClaimTaskRecovery(ctx context.Context, taskID int64, generation string, expectedOwner, ownerPID, limit int, now, nextAttemptAt int64) (bool, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_recovery(task_id,generation) VALUES(?,?)`, taskID, generation); err != nil {
		return false, err
	}
	// Completed episodes may restart on a real new generation. Pending and
	// exhausted episodes keep their budget even when generations change.
	if _, err := tx.ExecContext(ctx, `UPDATE task_recovery SET generation=?,attempts=0,next_attempt_at=0,status='pending' WHERE task_id=? AND status='recovered' AND generation<>?`, generation, taskID, generation); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE task_recovery SET generation=?,attempts=attempts+1,next_attempt_at=?,owner_pid=?,status='running' WHERE task_id=? AND owner_pid=? AND status IN ('pending','running') AND attempts<? AND next_attempt_at<=?`, generation, nextAttemptAt, ownerPID, taskID, expectedOwner, limit, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, tx.Commit()
}

// FinishTaskRecovery atomically stops an exhausted episode and raises exactly
// one actionable Notice, including when the final attempt's process crashed.
func (db *DB) FinishTaskRecovery(ctx context.Context, task Task, ownerPID, limit int, succeeded bool, cause string, nextAttemptAt int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts int
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT attempts,status FROM task_recovery WHERE task_id=? AND owner_pid=?`, task.ID, ownerPID).Scan(&attempts, &status); err != nil {
		return err
	}
	if status == "exhausted" || status == "recovered" && succeeded {
		return nil
	}
	nextStatus := "pending"
	if succeeded {
		nextStatus = "recovered"
	} else if attempts >= limit {
		nextStatus = "exhausted"
	}
	if succeeded {
		nextAttemptAt = 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_recovery SET owner_pid=0,status=?,last_error=?,next_attempt_at=? WHERE task_id=? AND owner_pid=?`, nextStatus, cause, nextAttemptAt, task.ID, ownerPID); err != nil {
		return err
	}
	if nextStatus == "exhausted" {
		summary := fmt.Sprintf("%s: automatic Rider recovery stopped after %d attempts; run posse relaunch t%d after inspecting the pane", task.Title, attempts, task.Seq)
		data, _ := json.Marshal(map[string]string{"error": cause})
		if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,'recovery_failed',?,?,?)`, task.ProjectID, task.ID, summary, string(data), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if nextStatus == "exhausted" {
		return db.PersistProject(ctx, task.ProjectID)
	}
	return nil
}

// RetryRecoveredTask reopens a completed episode only after its caller verifies
// that the recovered Rider pane is no longer live in the same generation.
func (db *DB) RetryRecoveredTask(ctx context.Context, taskID int64, generation string) (bool, error) {
	result, err := db.ExecContext(ctx, `UPDATE task_recovery SET status='pending',next_attempt_at=0 WHERE task_id=? AND generation=? AND owner_pid=0 AND status='recovered'`, taskID, generation)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (db *DB) ResetTaskRecovery(ctx context.Context, taskID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE task_recovery SET generation=(SELECT agent_server_started_at FROM tasks WHERE id=?),attempts=0,next_attempt_at=0,owner_pid=0,status='recovered',last_error='' WHERE task_id=?`, taskID, taskID)
	return err
}
