package store

import (
	"context"
	"encoding/json"
)

// RecordVerifiedPRHead retains a head that was checked against the Task's
// branch and forge before a later follow-up clears the Gate.
func (db *DB) RecordVerifiedPRHead(ctx context.Context, taskID int64, url, sha string) error {
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO verified_pr_heads(task_id, pr_url, head_sha) VALUES (?,?,?)`, taskID, url, sha)
	return err
}

func (db *DB) WasVerifiedPRHead(ctx context.Context, taskID int64, url, sha string) (bool, error) {
	var found bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM verified_pr_heads WHERE task_id=? AND pr_url=? AND head_sha=?)`, taskID, url, sha).Scan(&found)
	if err != nil || found {
		return found, err
	}
	// PRs opened before verified_pr_heads existed still have the head and URL
	// recorded in their pr_opened Notice from the verified Gate.
	rows, err := db.QueryContext(ctx, `SELECT data_json FROM notices WHERE task_id=? AND kind='pr_opened'`, taskID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return false, err
		}
		var opened struct {
			URL  string `json:"url"`
			Head string `json:"head_sha"`
		}
		if json.Unmarshal([]byte(data), &opened) == nil && opened.URL == url && opened.Head == sha {
			return true, nil
		}
	}
	return false, rows.Err()
}
