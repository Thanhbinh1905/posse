package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type ReviewCapGrant struct {
	TaskID     int64  `toml:"task_id"`
	DecisionID int64  `toml:"decision_id"`
	Round      int    `toml:"round"`
	ResultJSON string `toml:"result_json"`
	CreatedAt  int64  `toml:"created_at"`
}

func (db *DB) ReviewCapGrant(ctx context.Context, taskID int64) (ReviewCapGrant, error) {
	var grant ReviewCapGrant
	err := db.QueryRowContext(ctx, `SELECT task_id,decision_id,round,result_json,created_at FROM review_cap_grants WHERE task_id=?`, taskID).Scan(&grant.TaskID, &grant.DecisionID, &grant.Round, &grant.ResultJSON, &grant.CreatedAt)
	if err == sql.ErrNoRows {
		return grant, ErrNotFound
	}
	return grant, err
}

func (db *DB) RecordReviewCapGrant(ctx context.Context, grant ReviewCapGrant) (ReviewCapGrant, error) {
	if grant.TaskID < 1 || grant.DecisionID < 1 || grant.Round < 1 || grant.ResultJSON == "" {
		return ReviewCapGrant{}, fmt.Errorf("invalid final Review grant")
	}
	grant.CreatedAt = time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, `INSERT INTO review_cap_grants(task_id,decision_id,round,result_json,created_at) VALUES(?,?,?,?,?) ON CONFLICT(task_id) DO NOTHING`, grant.TaskID, grant.DecisionID, grant.Round, grant.ResultJSON, grant.CreatedAt); err != nil {
		return ReviewCapGrant{}, err
	}
	actual, err := db.ReviewCapGrant(ctx, grant.TaskID)
	if err != nil {
		return actual, err
	}
	if actual.DecisionID != grant.DecisionID {
		return actual, fmt.Errorf("final Review allowance already belongs to another Decision")
	}
	return actual, db.PersistTask(ctx, grant.TaskID)
}
