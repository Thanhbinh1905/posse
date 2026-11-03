package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type BudgetUsageRecord struct {
	TaskID      int64  `toml:"task_id"`
	Launch      int    `toml:"launch"`
	SessionID   string `toml:"session_id"`
	SessionKind string `toml:"session_kind"`
	Tokens      int64  `toml:"tokens"`
	LocalDay    string `toml:"local_day"`
	DayTokens   int64  `toml:"day_tokens"`
}

type BudgetCrossing struct {
	ProjectID int64  `toml:"project_id"`
	Scope     string `toml:"scope"`
	ScopeKey  string `toml:"scope_key"`
	TaskID    int64  `toml:"task_id"`
	CreatedAt int64  `toml:"created_at"`
}

// RecordTaskBudgetUsage stores the latest cumulative session count and returns
// this Task's total across launches and the Project's total for localDay.
func (db *DB) RecordTaskBudgetUsage(ctx context.Context, projectID, taskID int64, launch int, sessionID, kind, localDay string, tokens int64) (int64, int64, error) {
	if taskID < 1 || launch < 1 || sessionID == "" || kind == "" || localDay == "" || tokens < 0 {
		return 0, 0, fmt.Errorf("invalid budget usage observation")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var previous, dayTokens int64
	var previousDay, previousKind string
	var previousLaunch int
	err = tx.QueryRowContext(ctx, `SELECT tokens,local_day,day_tokens,launch,session_kind FROM task_budget_usage WHERE task_id=? AND session_id=?`, taskID, sessionID).Scan(&previous, &previousDay, &dayTokens, &previousLaunch, &previousKind)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, err
	}
	changed := err == sql.ErrNoRows || tokens > previous || previousDay != localDay || previousLaunch != launch || previousKind != kind
	if err == sql.ErrNoRows {
		dayTokens = 0
		var startedAt int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT at FROM transitions WHERE task_id=? AND to_state IN ('spawning','working') ORDER BY id DESC LIMIT 1),(SELECT created_at FROM tasks WHERE id=?),0)`, taskID, taskID).Scan(&startedAt); err != nil {
			return 0, 0, err
		}
		if startedAt > 0 && time.UnixMilli(startedAt).In(time.Local).Format(time.DateOnly) == localDay {
			dayTokens = tokens
		}
	} else if previousDay == localDay {
		if tokens > previous {
			dayTokens += tokens - previous
		}
	} else {
		dayTokens = 0
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_budget_usage(task_id,launch,session_id,session_kind,tokens,local_day,day_tokens) VALUES(?,?,?,?,?,?,?) ON CONFLICT(task_id,session_id) DO UPDATE SET launch=excluded.launch,session_kind=excluded.session_kind,tokens=MAX(task_budget_usage.tokens,excluded.tokens),local_day=excluded.local_day,day_tokens=excluded.day_tokens`, taskID, launch, sessionID, kind, tokens, localDay, dayTokens); err != nil {
		return 0, 0, err
	}
	var taskTotal, projectDaily int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(tokens),0) FROM task_budget_usage WHERE task_id=?`, taskID).Scan(&taskTotal); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(u.day_tokens),0) FROM task_budget_usage u JOIN tasks t ON t.id=u.task_id WHERE t.project_id=? AND u.local_day=?`, projectID, localDay).Scan(&projectDaily); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	if changed {
		if err := db.PersistTask(ctx, taskID); err != nil {
			return 0, 0, err
		}
	}
	return taskTotal, projectDaily, nil
}

func (db *DB) TaskBudgetUsage(ctx context.Context, taskID int64) ([]BudgetUsageRecord, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id,launch,session_id,session_kind,tokens,local_day,day_tokens FROM task_budget_usage WHERE task_id=? ORDER BY launch,session_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []BudgetUsageRecord
	for rows.Next() {
		var row BudgetUsageRecord
		if err := rows.Scan(&row.TaskID, &row.Launch, &row.SessionID, &row.SessionKind, &row.Tokens, &row.LocalDay, &row.DayTokens); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (db *DB) TaskBudgetCrossings(ctx context.Context, taskID int64) ([]BudgetCrossing, error) {
	rows, err := db.QueryContext(ctx, `SELECT project_id,scope,scope_key,task_id,created_at FROM budget_crossings WHERE task_id=? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []BudgetCrossing
	for rows.Next() {
		var row BudgetCrossing
		if err := rows.Scan(&row.ProjectID, &row.Scope, &row.ScopeKey, &row.TaskID, &row.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// CreateBudgetCrossingNotice atomically records one crossing and its Notice.
func (db *DB) CreateBudgetCrossingNotice(ctx context.Context, projectID, taskID int64, scope, key, summary, dataJSON string) (bool, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO budget_crossings(project_id,scope,scope_key,task_id,created_at) VALUES(?,?,?,?,?) ON CONFLICT(project_id,scope,scope_key) DO NOTHING`, projectID, scope, key, taskID, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 0 {
		return false, err
	}
	if dataJSON == "" {
		dataJSON = "{}"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?, ?, 'budget_exceeded', ?, ?, ?)`, projectID, taskID, summary, dataJSON, time.Now().UnixMilli()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return false, err
	}
	return true, nil
}
