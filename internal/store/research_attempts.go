package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrResearchBudget refuses a research child beyond its parent's budget.
var ErrResearchBudget = errors.New("research concurrency budget is exhausted")

// ResearchAttempt is one bounded native research child and its parent.
type ResearchAttempt struct {
	ID              int64
	ProjectID       int64
	TaskID          int64 // 0 for a Lead parent
	ParentRole      string
	ParentSession   string
	ParentPID       int
	ParentIdentity  string
	Harness         string
	HarnessVersion  string
	IdempotencyKey  string
	PreviousID      int64
	ChildID         string
	AgentType       string
	Model           string
	Tools           []string
	Access          string
	Scope           string
	PromptDigest    string
	State           string
	Resumable       bool
	SessionRef      string
	RunnerPID       int
	RunnerIdentity  string
	ChildPID        int
	ChildIdentity   string
	Usage           string
	ResultPath      string
	ResultBytes     int64
	ResultDigest    string
	ResultTruncated bool
	Note            string
	CreatedAt       int64
	UpdatedAt       int64
	EndedAt         int64
}

const researchAttemptColumns = `id, project_id, COALESCE(task_id, 0), parent_role, parent_session, parent_pid, parent_identity,
	harness, harness_version, idempotency_key, COALESCE(previous_id, 0), child_id, agent_type, model, tools, access, scope,
	prompt_digest, state, resumable, session_ref, runner_pid, runner_identity, child_pid, child_identity, usage, result_path, result_bytes,
	result_digest, result_truncated, note, created_at, updated_at, COALESCE(ended_at, 0)`

func scanResearchAttempt(row interface{ Scan(...any) error }) (ResearchAttempt, error) {
	var attempt ResearchAttempt
	var tools string
	err := row.Scan(&attempt.ID, &attempt.ProjectID, &attempt.TaskID, &attempt.ParentRole, &attempt.ParentSession, &attempt.ParentPID, &attempt.ParentIdentity,
		&attempt.Harness, &attempt.HarnessVersion, &attempt.IdempotencyKey, &attempt.PreviousID, &attempt.ChildID, &attempt.AgentType, &attempt.Model, &tools, &attempt.Access, &attempt.Scope,
		&attempt.PromptDigest, &attempt.State, &attempt.Resumable, &attempt.SessionRef, &attempt.RunnerPID, &attempt.RunnerIdentity, &attempt.ChildPID, &attempt.ChildIdentity, &attempt.Usage, &attempt.ResultPath, &attempt.ResultBytes,
		&attempt.ResultDigest, &attempt.ResultTruncated, &attempt.Note, &attempt.CreatedAt, &attempt.UpdatedAt, &attempt.EndedAt)
	if err != nil {
		return ResearchAttempt{}, err
	}
	_ = json.Unmarshal([]byte(tools), &attempt.Tools)
	return attempt, nil
}

// CreateResearchAttempt registers a child before it may run. A repeated
// idempotency key returns the existing attempt with created=false, so a
// reloaded parent never duplicates its child. maxActive bounds the parent's
// registered and running attempts; a refused attempt is recorded in its state.
func (db *DB) CreateResearchAttempt(ctx context.Context, attempt ResearchAttempt, maxActive int) (ResearchAttempt, bool, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return ResearchAttempt{}, false, err
	}
	defer tx.Rollback()
	if attempt.IdempotencyKey != "" {
		existing, err := scanResearchAttempt(tx.QueryRowContext(ctx, `SELECT `+researchAttemptColumns+` FROM research_attempts WHERE project_id=? AND harness=? AND idempotency_key=?`, attempt.ProjectID, attempt.Harness, attempt.IdempotencyKey))
		if err == nil {
			return existing, false, tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ResearchAttempt{}, false, err
		}
	}
	if attempt.State != "refused" && maxActive > 0 {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM research_attempts WHERE project_id=? AND COALESCE(task_id, 0)=? AND state IN ('registered', 'running')`, attempt.ProjectID, attempt.TaskID).Scan(&active); err != nil {
			return ResearchAttempt{}, false, err
		}
		if active >= maxActive {
			return ResearchAttempt{}, false, fmt.Errorf("%w: %d of %d research children are active for this parent", ErrResearchBudget, active, maxActive)
		}
	}
	tools, _ := json.Marshal(attempt.Tools)
	if attempt.Tools == nil {
		tools = []byte("[]")
	}
	if attempt.Usage == "" {
		attempt.Usage = "{}"
	}
	if attempt.Access == "" {
		attempt.Access = "read-only"
	}
	now := time.Now().UnixMilli()
	var ended any
	if attempt.State == "refused" {
		ended = now
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO research_attempts(project_id, task_id, parent_role, parent_session, parent_pid, parent_identity, harness, harness_version,
		idempotency_key, previous_id, child_id, agent_type, model, tools, access, scope, prompt_digest, state, resumable, session_ref, usage, note, created_at, updated_at, ended_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		attempt.ProjectID, nullableID(attempt.TaskID), attempt.ParentRole, attempt.ParentSession, attempt.ParentPID, attempt.ParentIdentity, attempt.Harness, attempt.HarnessVersion,
		attempt.IdempotencyKey, nullableID(attempt.PreviousID), attempt.ChildID, attempt.AgentType, attempt.Model, string(tools), attempt.Access, attempt.Scope, attempt.PromptDigest,
		attempt.State, attempt.Resumable, attempt.SessionRef, attempt.Usage, attempt.Note, now, now, ended)
	if err != nil {
		return ResearchAttempt{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ResearchAttempt{}, false, err
	}
	created, err := scanResearchAttempt(tx.QueryRowContext(ctx, `SELECT `+researchAttemptColumns+` FROM research_attempts WHERE id=?`, id))
	if err != nil {
		return ResearchAttempt{}, false, err
	}
	return created, true, tx.Commit()
}

func (db *DB) ResearchAttempt(ctx context.Context, id int64) (ResearchAttempt, error) {
	return scanResearchAttempt(db.QueryRowContext(ctx, `SELECT `+researchAttemptColumns+` FROM research_attempts WHERE id=?`, id))
}

// ResearchAttemptFilter selects attempts of one Project. Task 0 with Lead set
// selects the Lead's own children; Active limits to registered and running.
type ResearchAttemptFilter struct {
	ProjectID int64
	TaskID    int64
	Lead      bool
	Active    bool
	Harness   string
	Limit     int
}

func (db *DB) ResearchAttempts(ctx context.Context, filter ResearchAttemptFilter) ([]ResearchAttempt, error) {
	query := `SELECT ` + researchAttemptColumns + ` FROM research_attempts WHERE project_id=?`
	args := []any{filter.ProjectID}
	switch {
	case filter.TaskID != 0:
		query += ` AND task_id=?`
		args = append(args, filter.TaskID)
	case filter.Lead:
		query += ` AND task_id IS NULL`
	}
	if filter.Active {
		query += ` AND state IN ('registered', 'running')`
	}
	if filter.Harness != "" {
		query += ` AND harness=?`
		args = append(args, filter.Harness)
	}
	query += ` ORDER BY id DESC`
	if filter.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, filter.Limit)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []ResearchAttempt{}
	for rows.Next() {
		attempt, err := scanResearchAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

// ActiveResearchAttempts lists registered and running attempts in every Project.
func (db *DB) ActiveResearchAttempts(ctx context.Context) ([]ResearchAttempt, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+researchAttemptColumns+` FROM research_attempts WHERE state IN ('registered', 'running') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []ResearchAttempt{}
	for rows.Next() {
		attempt, err := scanResearchAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

// ResearchAttemptByChild returns the newest attempt bound to a native child.
func (db *DB) ResearchAttemptByChild(ctx context.Context, harness, childID string) (ResearchAttempt, error) {
	if childID == "" {
		return ResearchAttempt{}, sql.ErrNoRows
	}
	return scanResearchAttempt(db.QueryRowContext(ctx, `SELECT `+researchAttemptColumns+` FROM research_attempts WHERE harness=? AND child_id=? ORDER BY id DESC LIMIT 1`, harness, childID))
}

// UnboundResearchAttempts lists registered Claude attempts of one parent
// session and agent type that have no child identity yet.
func (db *DB) UnboundResearchAttempts(ctx context.Context, harness, parentSession, agentType string) ([]ResearchAttempt, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+researchAttemptColumns+` FROM research_attempts WHERE harness=? AND parent_session=? AND agent_type=? AND state='registered' AND child_id='' ORDER BY id`, harness, parentSession, agentType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []ResearchAttempt{}
	for rows.Next() {
		attempt, err := scanResearchAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

// ResearchChange is a partial update. Nil fields are unchanged. ChildID binds
// only an unbound attempt or confirms the bound identity.
type ResearchChange struct {
	State           *string
	ChildID         *string
	Model           *string
	Resumable       *bool
	SessionRef      *string
	RunnerPID       *int
	RunnerIdentity  *string
	ChildPID        *int
	ChildIdentity   *string
	Usage           *string
	ResultPath      *string
	ResultBytes     *int64
	ResultDigest    *string
	ResultTruncated *bool
	Note            *string
}

// UpdateResearchAttempt applies change when the attempt is in one of from.
// It returns ErrStateRace when the attempt moved on concurrently or its child
// identity differs from the one being bound.
func (db *DB) UpdateResearchAttempt(ctx context.Context, id int64, from []string, change ResearchChange) (ResearchAttempt, error) {
	sets := []string{"updated_at=?"}
	now := time.Now().UnixMilli()
	args := []any{now}
	add := func(column string, value any) {
		sets = append(sets, column+"=?")
		args = append(args, value)
	}
	if change.State != nil {
		add("state", *change.State)
		if *change.State != "registered" && *change.State != "running" {
			add("ended_at", now)
		} else {
			sets = append(sets, "ended_at=NULL")
		}
	}
	if change.ChildID != nil {
		add("child_id", *change.ChildID)
	}
	if change.Model != nil {
		add("model", *change.Model)
	}
	if change.Resumable != nil {
		add("resumable", *change.Resumable)
	}
	if change.SessionRef != nil {
		add("session_ref", *change.SessionRef)
	}
	if change.RunnerPID != nil {
		add("runner_pid", *change.RunnerPID)
	}
	if change.RunnerIdentity != nil {
		add("runner_identity", *change.RunnerIdentity)
	}
	if change.ChildPID != nil {
		add("child_pid", *change.ChildPID)
	}
	if change.ChildIdentity != nil {
		add("child_identity", *change.ChildIdentity)
	}
	if change.Usage != nil {
		add("usage", *change.Usage)
	}
	if change.ResultPath != nil {
		add("result_path", *change.ResultPath)
	}
	if change.ResultBytes != nil {
		add("result_bytes", *change.ResultBytes)
	}
	if change.ResultDigest != nil {
		add("result_digest", *change.ResultDigest)
	}
	if change.ResultTruncated != nil {
		add("result_truncated", *change.ResultTruncated)
	}
	if change.Note != nil {
		add("note", *change.Note)
	}
	query := `UPDATE research_attempts SET ` + strings.Join(sets, ", ") + ` WHERE id=?`
	args = append(args, id)
	if len(from) > 0 {
		query += ` AND state IN (` + strings.TrimSuffix(strings.Repeat("?,", len(from)), ",") + `)`
		for _, state := range from {
			args = append(args, state)
		}
	}
	if change.ChildID != nil {
		query += ` AND (child_id='' OR child_id=?)`
		args = append(args, *change.ChildID)
	}
	result, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return ResearchAttempt{}, err
	}
	if count, err := result.RowsAffected(); err != nil {
		return ResearchAttempt{}, err
	} else if count == 0 {
		if _, lookupErr := db.ResearchAttempt(ctx, id); lookupErr != nil {
			return ResearchAttempt{}, lookupErr
		}
		return ResearchAttempt{}, ErrStateRace
	}
	return db.ResearchAttempt(ctx, id)
}

// CountActiveResearchAttempts counts registered and running children.
func (db *DB) CountActiveResearchAttempts(ctx context.Context) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM research_attempts WHERE state IN ('registered', 'running')`).Scan(&count)
	return count, err
}

// ResearchAttemptByKey returns the attempt registered under an idempotency key.
func (db *DB) ResearchAttemptByKey(ctx context.Context, projectID int64, harness, key string) (ResearchAttempt, error) {
	return scanResearchAttempt(db.QueryRowContext(ctx, `SELECT `+researchAttemptColumns+` FROM research_attempts WHERE project_id=? AND harness=? AND idempotency_key=?`, projectID, harness, key))
}
