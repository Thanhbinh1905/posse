package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const GreenhouseReviewProvider = "greenhouse"

// ErrVerdictHeadMoved refuses a Verdict whose reviewed head is no longer the
// Member's head when Posse records it.
var ErrVerdictHeadMoved = errors.New("the reviewed head is no longer the Task head")

type greenhouseReviewProvider struct{}

func (greenhouseReviewProvider) RecordVerdict(ctx context.Context, tx VerdictTx, verdict Verdict) error {
	if verdict.Provider != GreenhouseReviewProvider {
		return sql.ErrNoRows
	}
	return insertVerdict(ctx, tx, verdict)
}

// RecordGreenhouseVerdict inserts a Greenhouse Verdict once per grant. It reads
// the Member head inside the write transaction and refuses the insert when the
// head differs from the reviewed one; it does not rely on Greenhouse's
// `current` flag. It reports false when the grant already has a Verdict.
func (db *DB) RecordGreenhouseVerdict(ctx context.Context, verdict Verdict, currentHead func(context.Context) (string, error)) (bool, error) {
	if verdict.Grant == "" {
		return false, errors.New("a Greenhouse Verdict needs its grant")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM verdicts WHERE grant_id=?)`, verdict.Grant).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	head, err := currentHead(ctx)
	if err != nil {
		return false, err
	}
	if head != verdict.Head {
		return false, fmt.Errorf("%w: reviewed %s, now %s", ErrVerdictHeadMoved, verdict.Head, head)
	}
	if err := (greenhouseReviewProvider{}).RecordVerdict(ctx, tx, verdict); err != nil {
		return false, err
	}
	if len(verdict.Findings) > 0 {
		var verdictID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM verdicts WHERE grant_id=?`, verdict.Grant).Scan(&verdictID); err != nil {
			return false, err
		}
		if err := recordProviderFindingsTx(ctx, tx.Tx, verdictID, verdict); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (db *DB) GreenhouseFailureNoticeSummary(ctx context.Context, taskID int64, repository, head string) (string, bool, error) {
	data, err := json.Marshal(struct {
		Repository string `json:"repo"`
		Head       string `json:"head"`
	}{repository, head})
	if err != nil {
		return "", false, err
	}
	var summary string
	err = db.QueryRowContext(ctx, `SELECT summary FROM notices WHERE task_id=? AND kind='greenhouse_review_failed' AND data_json=?`, taskID, string(data)).Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return summary, err == nil, err
}

func (db *DB) CreateGreenhouseFailureNoticeOnce(ctx context.Context, projectID, taskID int64, repository, head, reason string) (bool, error) {
	data, err := json.Marshal(struct {
		Repository string `json:"repo"`
		Head       string `json:"head"`
	}{repository, head})
	if err != nil {
		return false, err
	}
	summary := fmt.Sprintf("Greenhouse parallel review failed for %s at %s: %s", repository, head, reason)
	updated, err := db.ExecContext(ctx, `UPDATE notices SET summary=? WHERE task_id=? AND kind='greenhouse_review_failed' AND data_json=? AND summary<>?`, summary, taskID, string(data), summary)
	if err != nil {
		return false, err
	}
	updatedRows, err := updated.RowsAffected()
	if err != nil {
		return false, err
	}
	if updatedRows > 0 {
		return false, db.PersistProject(ctx, projectID)
	}
	result, err := db.ExecContext(ctx, `INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at)
		SELECT ?, ?, 'greenhouse_review_failed', ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM notices WHERE task_id=? AND kind='greenhouse_review_failed' AND data_json=?)`,
		projectID, taskID, summary, string(data), time.Now().UnixMilli(), taskID, string(data))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 0 {
		return false, err
	}
	return true, db.PersistProject(ctx, projectID)
}

// CreateGreenhouseRecoveryNoticeOnce raises one Notice per parked Greenhouse
// recovery of a Task Member.
func (db *DB) CreateGreenhouseRecoveryNoticeOnce(ctx context.Context, projectID, taskID int64, repository, recoveryID, summary string) (bool, error) {
	data, err := json.Marshal(struct {
		Repository string `json:"repo"`
		Recovery   string `json:"recovery_id"`
	}{repository, recoveryID})
	if err != nil {
		return false, err
	}
	result, err := db.ExecContext(ctx, `INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at)
		SELECT ?, ?, 'greenhouse_recovery_required', ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM notices WHERE task_id=? AND kind='greenhouse_recovery_required' AND data_json=?)`,
		projectID, taskID, summary, string(data), time.Now().UnixMilli(), taskID, string(data))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 0 {
		return false, err
	}
	return true, db.PersistProject(ctx, projectID)
}
