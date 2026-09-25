package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var ErrIntentOwned = errors.New("Task already has an active intent")

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
	result, err := db.ExecContext(ctx, `DELETE FROM intents WHERE id=? AND process_id=?`, intentID, processID)
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

func isUniqueConstraint(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY constraint failed")
}
