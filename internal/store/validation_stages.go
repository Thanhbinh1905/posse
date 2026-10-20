package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ValidationStageResult records one local full-pipeline result bound to the
// candidate, frozen target and admitted-plan digest that it validated.
type ValidationStageResult struct {
	ID             int64  `toml:"id"`
	TaskID         int64  `toml:"task_id"`
	CandidateID    int64  `toml:"candidate_id"`
	Repo           string `toml:"repo,omitempty"`
	Stage          string `toml:"stage"`
	CandidateSHA   string `toml:"candidate_sha"`
	TargetSHA      string `toml:"target_sha"`
	ConfigSHA256   string `toml:"config_sha256"`
	Status         string `toml:"status"`
	Summary        string `toml:"summary"`
	Evidence       string `toml:"evidence"`
	Coverage       string `toml:"coverage,omitempty"`
	InvalidatedSHA string `toml:"invalidated_by_sha,omitempty"`
	CreatedAt      int64  `toml:"created_at"`
}

func (db *DB) RecordValidationStage(ctx context.Context, result ValidationStageResult) (ValidationStageResult, error) {
	if result.TaskID < 1 || result.CandidateID < 1 || result.Stage != "test" && result.Stage != "docs" && result.Stage != "lint" ||
		!validValidationSHA(result.CandidateSHA) || !validValidationSHA(result.TargetSHA) || len(result.ConfigSHA256) != 64 ||
		result.Summary == "" || result.Evidence == "" {
		return ValidationStageResult{}, fmt.Errorf("invalid validation stage result")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return ValidationStageResult{}, err
	}
	defer tx.Rollback()
	var candidateSHA, targetSHA, candidateState string
	if err := tx.QueryRowContext(ctx, `SELECT candidate_sha,target_sha,state FROM validation_candidates WHERE id=? AND task_id=?`, result.CandidateID, result.TaskID).Scan(&candidateSHA, &targetSHA, &candidateState); err != nil {
		return ValidationStageResult{}, err
	}
	if candidateSHA != result.CandidateSHA || targetSHA != result.TargetSHA || candidateState != ValidationCandidateActive {
		return ValidationStageResult{}, ErrStateRace
	}
	if result.Stage == "docs" || result.Stage == "lint" {
		previous := "test"
		if result.Stage == "lint" {
			previous = "docs"
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM validation_stage_results WHERE candidate_id=? AND stage=? AND candidate_sha=? AND target_sha=? AND config_sha256=? AND status='passed')`,
			result.CandidateID, previous, result.CandidateSHA, result.TargetSHA, result.ConfigSHA256).Scan(&exists); err != nil {
			return ValidationStageResult{}, err
		}
		if !exists {
			return ValidationStageResult{}, fmt.Errorf("validation stage %s requires a passing %s result for the same candidate", result.Stage, previous)
		}
	}
	var existing ValidationStageResult
	err = tx.QueryRowContext(ctx, `SELECT id,task_id,candidate_id,repo,stage,candidate_sha,target_sha,config_sha256,status,summary,evidence,coverage,invalidated_by_sha,created_at FROM validation_stage_results WHERE candidate_id=? AND stage=? AND candidate_sha=?`, result.CandidateID, result.Stage, result.CandidateSHA).Scan(
		&existing.ID, &existing.TaskID, &existing.CandidateID, &existing.Repo, &existing.Stage, &existing.CandidateSHA, &existing.TargetSHA, &existing.ConfigSHA256,
		&existing.Status, &existing.Summary, &existing.Evidence, &existing.Coverage, &existing.InvalidatedSHA, &existing.CreatedAt)
	if err == nil {
		if existing.TargetSHA != result.TargetSHA || existing.ConfigSHA256 != result.ConfigSHA256 {
			return ValidationStageResult{}, fmt.Errorf("validation stage binding changed for the same candidate head")
		}
		if err := tx.Commit(); err != nil {
			return ValidationStageResult{}, err
		}
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return ValidationStageResult{}, err
	}
	result.Status = "passed"
	result.CreatedAt = time.Now().UnixMilli()
	insert, err := tx.ExecContext(ctx, `INSERT INTO validation_stage_results(task_id,candidate_id,repo,stage,candidate_sha,target_sha,config_sha256,status,summary,evidence,coverage,created_at) VALUES(?,?,?,?,?,?,?,'passed',?,?,?,?)`,
		result.TaskID, result.CandidateID, result.Repo, result.Stage, result.CandidateSHA, result.TargetSHA, result.ConfigSHA256, result.Summary, result.Evidence, result.Coverage, result.CreatedAt)
	if err != nil {
		return ValidationStageResult{}, err
	}
	result.ID, err = insert.LastInsertId()
	if err != nil {
		return ValidationStageResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ValidationStageResult{}, err
	}
	if err := db.PersistTask(ctx, result.TaskID); err != nil {
		return result, err
	}
	return result, nil
}

func (db *DB) InvalidateValidationStages(ctx context.Context, candidateID int64, invalidatedBySHA string) error {
	if candidateID < 1 || !validValidationSHA(invalidatedBySHA) {
		return fmt.Errorf("invalid validation stage invalidation")
	}
	result, err := db.ExecContext(ctx, `UPDATE validation_stage_results SET status='invalidated',invalidated_by_sha=? WHERE candidate_id=? AND status='passed'`, invalidatedBySHA, candidateID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return nil
	}
	var taskID int64
	if err := db.QueryRowContext(ctx, `SELECT task_id FROM validation_candidates WHERE id=?`, candidateID).Scan(&taskID); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) ValidationStages(ctx context.Context, taskID int64) ([]ValidationStageResult, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,task_id,candidate_id,repo,stage,candidate_sha,target_sha,config_sha256,status,summary,evidence,coverage,invalidated_by_sha,created_at FROM validation_stage_results WHERE task_id=? ORDER BY candidate_id,created_at,stage`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []ValidationStageResult
	for rows.Next() {
		var result ValidationStageResult
		if err := rows.Scan(&result.ID, &result.TaskID, &result.CandidateID, &result.Repo, &result.Stage, &result.CandidateSHA, &result.TargetSHA, &result.ConfigSHA256,
			&result.Status, &result.Summary, &result.Evidence, &result.Coverage, &result.InvalidatedSHA, &result.CreatedAt); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (db *DB) ValidationStage(ctx context.Context, candidateID int64, stage, candidateSHA, targetSHA, configSHA string) (ValidationStageResult, error) {
	var result ValidationStageResult
	err := db.QueryRowContext(ctx, `SELECT id,task_id,candidate_id,repo,stage,candidate_sha,target_sha,config_sha256,status,summary,evidence,coverage,invalidated_by_sha,created_at FROM validation_stage_results WHERE candidate_id=? AND stage=? AND candidate_sha=? AND target_sha=? AND config_sha256=? AND status='passed'`,
		candidateID, stage, candidateSHA, targetSHA, configSHA).Scan(&result.ID, &result.TaskID, &result.CandidateID, &result.Repo, &result.Stage, &result.CandidateSHA, &result.TargetSHA, &result.ConfigSHA256,
		&result.Status, &result.Summary, &result.Evidence, &result.Coverage, &result.InvalidatedSHA, &result.CreatedAt)
	if err == sql.ErrNoRows {
		return ValidationStageResult{}, ErrNotFound
	}
	return result, err
}

func validValidationSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				if character < 'A' || character > 'F' {
					return false
				}
			}
		}
	}
	return true
}
