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
	err = db.QueryRowContext(ctx, `SELECT summary FROM notices WHERE task_id=? AND kind IN ('greenhouse_review_failed','needs_decision') AND data_json=? ORDER BY id DESC LIMIT 1`, taskID, string(data)).Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return summary, err == nil, err
}

func (db *DB) CreateGreenhouseFailureNoticeOnce(ctx context.Context, projectID, taskID int64, repository, head, reason string) (bool, error) {
	data, err := greenhouseNoticeData(repository, head)
	if err != nil {
		return false, err
	}
	summary := fmt.Sprintf("Greenhouse parallel review failed for %s at %s: %s", repository, head, reason)
	updated, err := db.ExecContext(ctx, `UPDATE notices SET summary=? WHERE task_id=? AND kind='greenhouse_review_failed' AND data_json=? AND summary<>?`, summary, taskID, data, summary)
	if err != nil {
		return false, err
	}
	rows, err := updated.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows > 0 {
		return false, db.PersistProject(ctx, projectID)
	}
	result, err := db.ExecContext(ctx, `INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at)
		SELECT ?, ?, 'greenhouse_review_failed', ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM notices WHERE task_id=? AND kind='greenhouse_review_failed' AND data_json=?)`,
		projectID, taskID, summary, data, time.Now().UnixMilli(), taskID, data)
	if err != nil {
		return false, err
	}
	rows, err = result.RowsAffected()
	if err != nil || rows == 0 {
		return false, err
	}
	return true, db.PersistProject(ctx, projectID)
}

func (db *DB) CreateGreenhouseNeedsDecisionNoticeOnce(ctx context.Context, projectID, taskID int64, repository, head, reason string) (bool, error) {
	data, err := greenhouseNoticeData(repository, head)
	if err != nil {
		return false, err
	}
	summary := fmt.Sprintf("Greenhouse review failed for %s at %s: %s", repository, head, reason)
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `UPDATE notices SET claim_token='',claimed_at=0 WHERE claim_token IN (
		SELECT r.owner_token FROM notice_delivery_receipts r WHERE r.project_id=? AND r.state IN ('claimed','printed') AND EXISTS (
			SELECT 1 FROM json_each(r.notice_ids_json) ids JOIN notices n ON n.id=CAST(ids.value AS INTEGER)
			WHERE n.task_id=? AND n.kind IN ('greenhouse_review_failed','needs_decision') AND n.data_json=?))`, projectID, taskID, data)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE notice_delivery_receipts SET state='rejected',owner_token='',lease_until=0,updated_at=? WHERE project_id=? AND state IN ('claimed','printed') AND EXISTS (
		SELECT 1 FROM json_each(notice_delivery_receipts.notice_ids_json) ids JOIN notices n ON n.id=CAST(ids.value AS INTEGER)
		WHERE n.task_id=? AND n.kind IN ('greenhouse_review_failed','needs_decision') AND n.data_json=?)`, time.Now().UnixMilli(), projectID, taskID, data)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notices SET acked_at=COALESCE(acked_at,?),claim_token=CASE WHEN claim_token LIKE 'uncertain:%' THEN claim_token ELSE '' END,claimed_at=CASE WHEN claim_token LIKE 'uncertain:%' THEN claimed_at ELSE 0 END WHERE project_id=? AND task_id=? AND kind='greenhouse_review_failed' AND data_json=?`, time.Now().UnixMilli(), projectID, taskID, data); err != nil {
		return false, err
	}
	var noticeID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM notices WHERE task_id=? AND kind='needs_decision' AND data_json=?`, taskID, data).Scan(&noticeID)
	created := false
	if errors.Is(err, sql.ErrNoRows) {
		result, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,'needs_decision',?,?,?)`, projectID, taskID, summary, data, time.Now().UnixMilli())
		if err != nil {
			return false, err
		}
		noticeID, err = result.LastInsertId()
		if err != nil {
			return false, err
		}
		created = true
	} else if err != nil {
		return false, err
	} else if _, err := tx.ExecContext(ctx, `UPDATE notices SET summary=? WHERE id=?`, summary, noticeID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if err := db.PersistProject(ctx, projectID); err != nil {
		return false, err
	}
	if err := db.PersistNoticeDeliverySnapshot(ctx, projectID); err != nil {
		return false, err
	}
	return created, nil
}

func greenhouseNoticeData(repository, head string) (string, error) {
	data, err := json.Marshal(struct {
		Repository string `json:"repo"`
		Head       string `json:"head"`
	}{repository, head})
	return string(data), err
}

func (db *DB) CreateGreenhousePartialNoticeOnce(ctx context.Context, projectID, taskID int64, repository, head, summary string, result any) (bool, error) {
	data, err := json.Marshal(struct {
		Repository string `json:"repo"`
		Head       string `json:"head"`
		Result     any    `json:"result"`
	}{repository, head, result})
	if err != nil {
		return false, err
	}
	inserted, err := db.ExecContext(ctx, `INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at)
		SELECT ?, ?, 'greenhouse_partial_review', ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM notices WHERE task_id=? AND kind='greenhouse_partial_review' AND json_extract(data_json,'$.repo')=? AND json_extract(data_json,'$.head')=?)`,
		projectID, taskID, summary, string(data), time.Now().UnixMilli(), taskID, repository, head)
	if err != nil {
		return false, err
	}
	rows, err := inserted.RowsAffected()
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
