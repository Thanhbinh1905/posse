package store

import "context"

// ReviewReceipt is a pipeline-owned no-mistakes review receipt that qualified
// one exact Change candidate before no-mistakes was removed. Posse no longer
// records receipts; stored ones stay readable as Task history.
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
