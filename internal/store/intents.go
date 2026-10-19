package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	dbgen "github.com/thanhbinh1905/posse/internal/store/sqlc"
)

var ErrIntentOwned = errors.New("Task already has an active intent")
var ErrResourceLeaseHeld = errors.New("resource leases still require custody reconciliation")

type Intent struct {
	ID               int64
	ProjectID        int64
	TaskID           int64
	Command          string
	Step             string
	ProcessID        int
	ProcessBootID    string
	ProcessStartTime string
	UpdatedAt        int64
	PayloadJSON      string
}

func (db *DB) StartIntent(ctx context.Context, projectID, taskID int64, command, step, payload string, processID int) error {
	if command == "" || step == "" || processID < 1 {
		return fmt.Errorf("intent command, step and process id are required")
	}
	if payload == "" {
		payload = "{}"
	}
	bootID, startTime, _ := ProcessIdentityForPID(processID)
	_, err := db.ExecContext(ctx, `INSERT INTO intents(project_id,task_id,command,step,process_id,process_boot_id,process_start_time,updated_at,payload_json) VALUES(?,?,?,?,?,?,?,?,?)`, projectID, taskID, command, step, processID, bootID, startTime, time.Now().UnixMilli(), payload)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrIntentOwned
		}
		return err
	}
	return nil
}

func (db *DB) IntentByTask(ctx context.Context, taskID int64) (Intent, error) {
	var intent Intent
	err := db.QueryRowContext(ctx, `SELECT id,project_id,task_id,command,step,process_id,process_boot_id,process_start_time,updated_at,payload_json FROM intents WHERE task_id=?`, taskID).Scan(&intent.ID, &intent.ProjectID, &intent.TaskID, &intent.Command, &intent.Step, &intent.ProcessID, &intent.ProcessBootID, &intent.ProcessStartTime, &intent.UpdatedAt, &intent.PayloadJSON)
	return intent, err
}

func (db *DB) Intents(ctx context.Context, projectID int64) ([]Intent, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,task_id,command,step,process_id,process_boot_id,process_start_time,updated_at,payload_json FROM intents WHERE project_id=? ORDER BY updated_at,id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intents []Intent
	for rows.Next() {
		var intent Intent
		if err := rows.Scan(&intent.ID, &intent.ProjectID, &intent.TaskID, &intent.Command, &intent.Step, &intent.ProcessID, &intent.ProcessBootID, &intent.ProcessStartTime, &intent.UpdatedAt, &intent.PayloadJSON); err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (db *DB) UpdateIntentStep(ctx context.Context, intentID int64, processID int, step string) error {
	result, err := db.ExecContext(ctx, `UPDATE intents SET step=?,updated_at=? WHERE id=? AND process_id=?`, step, time.Now().UnixMilli(), intentID, processID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrIntentOwned
	}
	return nil
}

// UpdateIntentPayload records retained cancellation identities before signals.
func (db *DB) UpdateIntentPayload(ctx context.Context, intentID int64, processID int, payload string) error {
	result, err := db.ExecContext(ctx, `UPDATE intents SET payload_json=?,updated_at=? WHERE id=? AND process_id=?`, payload, time.Now().UnixMilli(), intentID, processID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrIntentOwned
	}
	return nil
}

// RecordDiscardApprovalForIntent pins the first post-stop branch tips for an
// interrupted discard. The approval row and intent payload commit atomically,
// so retrying approval.record cannot authorize later branch movement.
func (db *DB) RecordDiscardApprovalForIntent(ctx context.Context, intentID int64, processID int, taskID int64, quote, branchSHA string) (string, error) {
	if strings.TrimSpace(quote) == "" {
		return "", fmt.Errorf("user approval quote is required")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var recordedTaskID int64
	var recordedProcessID int
	var payloadJSON string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,process_id,payload_json FROM intents WHERE id=?`, intentID).Scan(&recordedTaskID, &recordedProcessID, &payloadJSON); err != nil {
		return "", err
	}
	if recordedTaskID != taskID || recordedProcessID != processID {
		return "", ErrIntentOwned
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return "", err
	}
	if payload == nil {
		return "", fmt.Errorf("discard intent payload must be a JSON object")
	}
	var intentQuote string
	if err := json.Unmarshal(payload["user_quote"], &intentQuote); err != nil || intentQuote != quote {
		return "", fmt.Errorf("discard approval does not match the User quote recorded in its intent")
	}
	var recorded bool
	if raw := payload["discard_approval_recorded"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &recorded); err != nil {
			return "", err
		}
	}
	if recorded {
		var approvedSHA string
		if err := json.Unmarshal(payload["discard_approval_branch_sha"], &approvedSHA); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		if err := db.PersistTask(ctx, taskID); err != nil {
			return "", err
		}
		return approvedSHA, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO approvals(task_id,action,user_quote,branch_sha,at) VALUES(?,'discard',?,?,?)`, taskID, quote, branchSHA, time.Now().UnixMilli()); err != nil {
		return "", err
	}
	payload["discard_approval_recorded"] = json.RawMessage("true")
	branchSHAJSON, err := json.Marshal(branchSHA)
	if err != nil {
		return "", err
	}
	payload["discard_approval_branch_sha"] = branchSHAJSON
	updated, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE intents SET payload_json=?,updated_at=? WHERE id=? AND process_id=?`, string(updated), time.Now().UnixMilli(), intentID, processID)
	if err != nil {
		return "", err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err != nil {
			return "", err
		}
		return "", ErrIntentOwned
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return "", err
	}
	return branchSHA, nil
}

func (db *DB) ClaimIntent(ctx context.Context, intentID int64, oldProcessID, newProcessID int, step string) (bool, error) {
	bootID, startTime, _ := ProcessIdentityForPID(newProcessID)
	result, err := db.ExecContext(ctx, `UPDATE intents SET process_id=?,process_boot_id=?,process_start_time=?,step=?,updated_at=? WHERE id=? AND process_id=?`, newProcessID, bootID, startTime, step, time.Now().UnixMilli(), intentID, oldProcessID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func ProcessIdentityForPID(pid int) (string, string, error) {
	if pid < 1 {
		return "", "", fmt.Errorf("process id must be positive")
	}
	bootData, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", err
	}
	statData, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", "", err
	}
	closeParen := strings.LastIndexByte(string(statData), ')')
	if closeParen < 0 || closeParen+1 >= len(statData) {
		return "", "", fmt.Errorf("invalid process stat for %d", pid)
	}
	fields := strings.Fields(string(statData[closeParen+1:]))
	if len(fields) <= 19 {
		return "", "", fmt.Errorf("process stat for %d has no start time", pid)
	}
	return strings.TrimSpace(string(bootData)), fields[19], nil
}

func (db *DB) FinishIntent(ctx context.Context, intentID int64, processID int) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	leases, err := dbgen.New(tx).CountResourceLeasesByIntent(ctx, intentID)
	if err != nil {
		return err
	}
	if leases > 0 {
		return ErrResourceLeaseHeld
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM intents WHERE id=? AND process_id=?`, intentID, processID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrIntentOwned
	}
	return tx.Commit()
}

func isUniqueConstraint(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY constraint failed")
}
