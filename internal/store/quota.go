package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// QuotaExhaustion records that one Worker launch reported an exhausted quota
// or usage limit, and what Posse did about it.
type QuotaExhaustion struct {
	TaskID     int64  `json:"task_id"`
	Launch     int    `json:"launch"`
	Agent      string `json:"agent"`
	Profile    string `json:"profile"`
	Line       string `json:"line"`
	Resets     string `json:"resets,omitempty"`
	Status     string `json:"status"`
	Fallback   string `json:"fallback,omitempty"`
	Detail     string `json:"detail,omitempty"`
	OwnerPID   int    `json:"-"`
	ObservedAt int64  `json:"observed_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

const quotaExhaustionColumns = `SELECT task_id,launch,agent,profile,line,resets,status,fallback,detail,owner_pid,observed_at,updated_at FROM quota_exhaustions`

// QuotaDecisionState reports the Task states in which a Worker can still sit
// at its harness's quota message.
func QuotaDecisionState(state State) bool {
	switch state {
	case StateWorking, StateStalled, StateBlocked, StateNeedsDecision:
		return true
	default:
		return false
	}
}

// TaskQuotaExhaustion returns the record for one launch, or ErrNotFound.
func (db *DB) TaskQuotaExhaustion(ctx context.Context, taskID int64, launch int) (QuotaExhaustion, error) {
	return scanQuotaExhaustion(db.QueryRowContext(ctx, quotaExhaustionColumns+` WHERE task_id=? AND launch=?`, taskID, launch))
}

// TaskQuotaExhaustions returns every recorded launch of a Task, oldest first.
func (db *DB) TaskQuotaExhaustions(ctx context.Context, taskID int64) ([]QuotaExhaustion, error) {
	rows, err := db.QueryContext(ctx, quotaExhaustionColumns+` WHERE task_id=? ORDER BY launch`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []QuotaExhaustion{}
	for rows.Next() {
		record, err := scanQuotaExhaustion(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// ClaimQuotaExhaustion records a launch's quota report and makes the caller
// its only handler. The launch must still be the Task's current one in a state
// where its Worker can be relaunched. A claim whose owner died before
// recording an outcome passes to the next observer.
func (db *DB) ClaimQuotaExhaustion(ctx context.Context, record QuotaExhaustion, ownerPID int, ownerAlive func(int) bool) (bool, error) {
	if record.TaskID < 1 || record.Launch < 1 || record.Agent == "" || record.Profile == "" || record.Line == "" || ownerPID < 1 {
		return false, fmt.Errorf("quota exhaustion identity is incomplete")
	}
	now := time.Now().UnixMilli()
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var state State
	var launches int
	if err := tx.QueryRowContext(ctx, `SELECT state,launches FROM tasks WHERE id=?`, record.TaskID).Scan(&state, &launches); err != nil {
		return false, err
	}
	if launches != record.Launch || !QuotaDecisionState(state) || state == StateNeedsDecision {
		return false, tx.Commit()
	}
	// An earlier launch's handler that is still relaunching owns the pane; its
	// quota message is still on screen and is not this launch's.
	var earlierOwner int
	err = tx.QueryRowContext(ctx, `SELECT owner_pid FROM quota_exhaustions WHERE task_id=? AND launch<? AND status='claimed' ORDER BY launch DESC LIMIT 1`, record.TaskID, record.Launch).Scan(&earlierOwner)
	if err == nil && ownerAlive(earlierOwner) {
		return false, tx.Commit()
	} else if err != nil && !IsNotFound(err) {
		return false, err
	}
	existing, err := scanQuotaExhaustion(tx.QueryRowContext(ctx, quotaExhaustionColumns+` WHERE task_id=? AND launch=?`, record.TaskID, record.Launch))
	switch {
	case IsNotFound(err):
		if _, err := tx.ExecContext(ctx, `INSERT INTO quota_exhaustions(task_id,launch,agent,profile,line,resets,status,owner_pid,observed_at,updated_at) VALUES(?,?,?,?,?,?,'claimed',?,?,?)`,
			record.TaskID, record.Launch, record.Agent, record.Profile, record.Line, record.Resets, ownerPID, now, now); err != nil {
			return false, err
		}
		record.Status, record.ObservedAt = "claimed", now
		if err := insertQuotaSignal(ctx, tx, record, "quota_exhausted", "Worker harness reported an exhausted quota or usage limit: "+record.Line, now); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	case existing.Status != "claimed" || existing.OwnerPID == ownerPID || ownerAlive(existing.OwnerPID):
		return false, tx.Commit()
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE quota_exhaustions SET owner_pid=?,updated_at=? WHERE task_id=? AND launch=? AND status='claimed' AND owner_pid=?`, ownerPID, now, record.TaskID, record.Launch, existing.OwnerPID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// FinishQuotaExhaustion records the claimed launch's outcome once, with its
// Notice in the same transaction. A fallback relaunch that failed after
// advancing the Task's launch leaves that launch without an agent; the outcome
// is also recorded for the current launch so its stale screen is not observed
// again and the Notice binds to it.
func (db *DB) FinishQuotaExhaustion(ctx context.Context, taskID int64, launch, ownerPID int, status, fallback, detail string, notice Notice) (*Notice, error) {
	verb := map[string]string{"fallback": "quota_fallback", "decision": "quota_decision"}[status]
	if verb == "" {
		return nil, fmt.Errorf("invalid quota exhaustion status %q", status)
	}
	if notice.ProjectID < 1 || notice.TaskID != taskID || notice.Kind == "" || notice.Summary == "" {
		return nil, fmt.Errorf("quota exhaustion Notice identity is incomplete")
	}
	now := time.Now().UnixMilli()
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE quota_exhaustions SET status=?,fallback=?,detail=?,updated_at=? WHERE task_id=? AND launch=? AND status='claimed' AND owner_pid=?`, status, fallback, detail, now, taskID, launch, ownerPID)
	if err != nil {
		return nil, err
	}
	if updated, err := result.RowsAffected(); err != nil || updated == 0 {
		return nil, err
	}
	record, err := scanQuotaExhaustion(tx.QueryRowContext(ctx, quotaExhaustionColumns+` WHERE task_id=? AND launch=?`, taskID, launch))
	if err != nil {
		return nil, err
	}
	if err := insertQuotaSignal(ctx, tx, record, verb, detail, now); err != nil {
		return nil, err
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT launches FROM tasks WHERE id=?`, taskID).Scan(&current); err != nil {
		return nil, err
	}
	if status == "decision" && current > launch {
		if _, err := tx.ExecContext(ctx, `INSERT INTO quota_exhaustions(task_id,launch,agent,profile,line,resets,status,fallback,detail,owner_pid,observed_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(task_id,launch) DO NOTHING`, taskID, current, record.Agent, record.Profile, record.Line, record.Resets, status, fallback, detail, ownerPID, record.ObservedAt, now); err != nil {
			return nil, err
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err == nil && data != nil {
			data["launch"] = current
			encoded, err := json.Marshal(data)
			if err != nil {
				return nil, err
			}
			notice.DataJSON = string(encoded)
		}
	}
	if notice.DataJSON == "" {
		notice.DataJSON = "{}"
	}
	notice.CreatedAt = now
	inserted, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,?,?,?,?)`, notice.ProjectID, notice.TaskID, notice.Kind, notice.Summary, notice.DataJSON, now)
	if err != nil {
		return nil, err
	}
	if notice.ID, err = inserted.LastInsertId(); err != nil {
		return nil, err
	}
	return &notice, tx.Commit()
}

func insertQuotaSignal(ctx context.Context, tx *writeTx, record QuotaExhaustion, verb, note string, now int64) error {
	data, err := json.Marshal(map[string]any{"launch": record.Launch, "agent": record.Agent, "profile": record.Profile, "line": record.Line, "resets": record.Resets, "fallback": record.Fallback})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO signals(task_id,verb,note,data_json,at) VALUES(?,?,?,?,?)`, record.TaskID, verb, note, string(data), now)
	return err
}

func scanQuotaExhaustion(row interface{ Scan(...any) error }) (QuotaExhaustion, error) {
	var record QuotaExhaustion
	err := row.Scan(&record.TaskID, &record.Launch, &record.Agent, &record.Profile, &record.Line, &record.Resets, &record.Status, &record.Fallback, &record.Detail, &record.OwnerPID, &record.ObservedAt, &record.UpdatedAt)
	return record, err
}
