package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
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

type TaskLaunchUsage struct {
	TaskID      int64    `json:"task_id"`
	Launch      int      `json:"launch"`
	SessionID   string   `json:"session_id"`
	SessionKind string   `json:"session_kind"`
	Input       int64    `json:"input_tokens"`
	Output      int64    `json:"output_tokens"`
	CacheRead   int64    `json:"cache_read_tokens"`
	CacheWrite  int64    `json:"cache_write_tokens"`
	CostUSD     *float64 `json:"cost_usd"`
	Profile     string   `json:"profile"`
}

type TaskReviewUsage struct {
	RequestID  int64
	TaskID     int64
	Repository string
	Head       string
	Grant      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	CostUSD    *float64
	UsageJSON  string
}

func (db *DB) TaskReviewUsages(ctx context.Context, taskID int64) ([]TaskReviewUsage, error) {
	rows, err := db.QueryContext(ctx, `SELECT request_id,task_id,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_usd,usage_json FROM task_review_usage WHERE task_id=? ORDER BY request_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TaskReviewUsage
	for rows.Next() {
		var row TaskReviewUsage
		var cost sql.NullFloat64
		if err := rows.Scan(&row.RequestID, &row.TaskID, &row.Input, &row.Output, &row.CacheRead, &row.CacheWrite, &cost, &row.UsageJSON); err != nil {
			return nil, err
		}
		if cost.Valid {
			row.CostUSD = &cost.Float64
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (db *DB) RecordTaskReviewUsage(ctx context.Context, usage TaskReviewUsage) error {
	if usage.TaskID < 1 || usage.Repository == "" || usage.Head == "" || usage.UsageJSON == "" || usage.Input < 0 || usage.Output < 0 || usage.CacheRead < 0 || usage.CacheWrite < 0 || usage.CostUSD != nil && (*usage.CostUSD < 0 || math.IsNaN(*usage.CostUSD) || math.IsInf(*usage.CostUSD, 0)) {
		return fmt.Errorf("invalid Task review usage")
	}
	result, err := db.ExecContext(ctx, `INSERT INTO task_review_usage(request_id,task_id,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_usd,usage_json) SELECT id,task_id,?,?,?,?,?,? FROM greenhouse_review_requests WHERE task_id=? AND repo=? AND requested_head=? AND COALESCE(grant_id,'')=? ORDER BY id DESC LIMIT 1 ON CONFLICT(request_id) DO UPDATE SET input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,cache_read_tokens=excluded.cache_read_tokens,cache_write_tokens=excluded.cache_write_tokens,cost_usd=excluded.cost_usd,usage_json=excluded.usage_json`, usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite, usage.CostUSD, usage.UsageJSON, usage.TaskID, usage.Repository, usage.Head, usage.Grant)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("greenhouse review request not found for usage")
	}
	return db.PersistTask(ctx, usage.TaskID)
}

type TaskUsageTotal struct {
	Input      int64    `json:"input_tokens"`
	Output     int64    `json:"output_tokens"`
	CacheRead  int64    `json:"cache_read_tokens"`
	CacheWrite int64    `json:"cache_write_tokens"`
	CostUSD    *float64 `json:"cost_usd"`
	KnownCost  bool     `json:"known_cost"`
	Known      bool     `json:"known"`
}

func (db *DB) RecordTaskLaunchUsage(ctx context.Context, record TaskLaunchUsage) error {
	if record.TaskID < 1 || record.Launch < 1 || record.SessionID == "" || record.SessionKind == "" || record.Input < 0 || record.Output < 0 || record.CacheRead < 0 || record.CacheWrite < 0 || record.CostUSD != nil && (*record.CostUSD < 0 || math.IsNaN(*record.CostUSD) || math.IsInf(*record.CostUSD, 0)) {
		return fmt.Errorf("invalid Task launch usage")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO task_launch_usage(task_id,launch,session_id,session_kind,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_usd,profile,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(task_id,launch,session_id) DO UPDATE SET input_tokens=MAX(task_launch_usage.input_tokens,excluded.input_tokens),output_tokens=MAX(task_launch_usage.output_tokens,excluded.output_tokens),cache_read_tokens=MAX(task_launch_usage.cache_read_tokens,excluded.cache_read_tokens),cache_write_tokens=MAX(task_launch_usage.cache_write_tokens,excluded.cache_write_tokens),cost_usd=CASE WHEN excluded.cost_usd IS NULL THEN NULL ELSE MAX(COALESCE(task_launch_usage.cost_usd,0),excluded.cost_usd) END,profile=excluded.profile,updated_at=excluded.updated_at`, record.TaskID, record.Launch, record.SessionID, record.SessionKind, record.Input, record.Output, record.CacheRead, record.CacheWrite, record.CostUSD, record.Profile, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return db.PersistTask(ctx, record.TaskID)
}

func (db *DB) TaskLaunchUsages(ctx context.Context, taskID int64) ([]TaskLaunchUsage, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id,launch,session_id,session_kind,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_usd,profile FROM task_launch_usage WHERE task_id=? ORDER BY launch,session_id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TaskLaunchUsage
	for rows.Next() {
		var row TaskLaunchUsage
		var cost sql.NullFloat64
		if err := rows.Scan(&row.TaskID, &row.Launch, &row.SessionID, &row.SessionKind, &row.Input, &row.Output, &row.CacheRead, &row.CacheWrite, &cost, &row.Profile); err != nil {
			return nil, err
		}
		if cost.Valid {
			row.CostUSD = &cost.Float64
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

type ReviewUsageTotal struct {
	CostUSD *float64
	Known   bool
	Unknown int
}

func (db *DB) ProjectReviewUsageTotal(ctx context.Context, projectID, since int64) (ReviewUsageTotal, error) {
	var total ReviewUsageTotal
	var cost sql.NullFloat64
	var requests, known int
	err := db.QueryRowContext(ctx, `SELECT COUNT(r.id),COUNT(u.request_id),SUM(u.cost_usd),COALESCE(SUM(CASE WHEN r.id IS NOT NULL AND (u.request_id IS NULL OR u.cost_usd IS NULL) THEN 1 ELSE 0 END),0) FROM greenhouse_review_requests r JOIN tasks t ON t.id=r.task_id LEFT JOIN task_review_usage u ON u.request_id=r.id WHERE t.project_id=? AND r.requested_at>=?`, projectID, since).Scan(&requests, &known, &cost, &total.Unknown)
	if err != nil {
		return ReviewUsageTotal{}, err
	}
	total.Known = requests > 0 && known == requests && total.Unknown == 0 && cost.Valid
	if total.Known {
		total.CostUSD = &cost.Float64
	}
	return total, nil
}

func (db *DB) TaskUsageTotal(ctx context.Context, taskID int64) (TaskUsageTotal, error) {
	var total TaskUsageTotal
	var cost sql.NullFloat64
	var unknown, records int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cache_write_tokens),0),SUM(cost_usd),COALESCE(SUM(CASE WHEN cost_usd IS NULL THEN 1 ELSE 0 END),0),COUNT(*) FROM task_launch_usage WHERE task_id=?`, taskID).Scan(&total.Input, &total.Output, &total.CacheRead, &total.CacheWrite, &cost, &unknown, &records)
	if err != nil {
		return TaskUsageTotal{}, err
	}
	var reviewInput, reviewOutput, reviewRead, reviewWrite, reviewRecords, reviewUnknown int64
	var reviewCost sql.NullFloat64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(u.input_tokens),0),COALESCE(SUM(u.output_tokens),0),COALESCE(SUM(u.cache_read_tokens),0),COALESCE(SUM(u.cache_write_tokens),0),COUNT(u.request_id),COALESCE(SUM(CASE WHEN r.id IS NOT NULL AND (u.request_id IS NULL OR u.cost_usd IS NULL) THEN 1 ELSE 0 END),0),SUM(u.cost_usd) FROM greenhouse_review_requests r LEFT JOIN task_review_usage u ON u.request_id=r.id WHERE r.task_id=?`, taskID).Scan(&reviewInput, &reviewOutput, &reviewRead, &reviewWrite, &reviewRecords, &reviewUnknown, &reviewCost); err != nil {
		return TaskUsageTotal{}, err
	}
	total.Input += reviewInput
	total.Output += reviewOutput
	total.CacheRead += reviewRead
	total.CacheWrite += reviewWrite
	total.Known = records > 0 || reviewRecords > 0
	if cost.Valid && reviewCost.Valid {
		value := cost.Float64 + reviewCost.Float64
		cost.Float64 = value
		cost.Valid = true
	} else if reviewCost.Valid {
		cost = reviewCost
	}
	unknown += int(reviewUnknown)
	if unknown == 0 && cost.Valid {
		total.CostUSD = &cost.Float64
		total.KnownCost = true
	}
	return total, nil
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
