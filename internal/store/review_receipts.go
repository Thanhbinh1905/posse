package store

import (
	"context"
	"fmt"
)

// ReviewReceipt is a pipeline-owned no-mistakes review receipt that qualified
// one exact Change candidate. Posse only computes ReceiptSHA256 and ObservedAt.
type ReviewReceipt struct {
	TaskID          int64  `toml:"task_id" json:"task_id"`
	Repository      string `toml:"repository" json:"repository"`
	RunID           string `toml:"run_id" json:"run_id"`
	HeadSHA         string `toml:"head_sha" json:"head_sha"`
	ReviewStatus    string `toml:"review_status" json:"review_status"`
	ReviewerProfile string `toml:"reviewer_profile" json:"reviewer_profile"`
	ReviewerModel   string `toml:"reviewer_model" json:"reviewer_model"`
	ReportSHA256    string `toml:"report_sha256" json:"report_sha256"`
	ReceiptSHA256   string `toml:"receipt_sha256" json:"receipt_sha256"`
	ObservedAt      int64  `toml:"observed_at" json:"observed_at"`
}

// RecordReviewReceipt stores a qualifying receipt once and refreshes the Task
// snapshot. The same receipt bytes observed again keep the first record.
func (db *DB) RecordReviewReceipt(ctx context.Context, receipt ReviewReceipt) error {
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO review_receipts(task_id,repository,run_id,head_sha,review_status,reviewer_profile,reviewer_model,report_sha256,receipt_sha256,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		receipt.TaskID, receipt.Repository, receipt.RunID, receipt.HeadSHA, receipt.ReviewStatus, receipt.ReviewerProfile, receipt.ReviewerModel, receipt.ReportSHA256, receipt.ReceiptSHA256, receipt.ObservedAt); err != nil {
		return fmt.Errorf("record review receipt for Task %d: %w", receipt.TaskID, err)
	}
	return db.PersistTask(ctx, receipt.TaskID)
}

// ReviewReceipts lists the receipts recorded for a Change Task, oldest first.
func (db *DB) ReviewReceipts(ctx context.Context, taskID int64) ([]ReviewReceipt, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id,repository,run_id,head_sha,review_status,reviewer_profile,reviewer_model,report_sha256,receipt_sha256,observed_at FROM review_receipts WHERE task_id=? ORDER BY observed_at, receipt_sha256`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var receipts []ReviewReceipt
	for rows.Next() {
		var receipt ReviewReceipt
		if err := rows.Scan(&receipt.TaskID, &receipt.Repository, &receipt.RunID, &receipt.HeadSHA, &receipt.ReviewStatus, &receipt.ReviewerProfile, &receipt.ReviewerModel, &receipt.ReportSHA256, &receipt.ReceiptSHA256, &receipt.ObservedAt); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
}
