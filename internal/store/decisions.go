package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// DecisionRequest identifies one question by an origin unique within a Project.
// Repeating the same origin returns the original Decision, even after it is answered.
type DecisionRequest struct {
	ProjectID int64
	TaskID    int64
	Origin    string
	Question  string
	Options   []string
}

type Decision struct {
	ID         int64    `json:"id"`
	ProjectID  int64    `json:"project_id"`
	TaskID     int64    `json:"task_id"`
	Origin     string   `json:"origin"`
	Question   string   `json:"question"`
	Options    []string `json:"options"`
	Answer     string   `json:"answer"`
	UserQuote  string   `json:"user_quote"`
	CreatedAt  int64    `json:"created_at"`
	AnsweredAt int64    `json:"answered_at"`
}

func (db *DB) RaiseDecision(ctx context.Context, request DecisionRequest) (Decision, error) {
	if request.ProjectID < 1 || request.TaskID < 1 || strings.TrimSpace(request.Origin) == "" || strings.TrimSpace(request.Question) == "" || len(request.Options) < 2 {
		return Decision{}, fmt.Errorf("Decision requires a Project, Task, origin, question and at least two options")
	}
	seen := map[string]bool{}
	for _, option := range request.Options {
		if strings.TrimSpace(option) != option || option == "" || seen[option] {
			return Decision{}, fmt.Errorf("Decision options must be distinct and non-empty")
		}
		seen[option] = true
	}
	options, err := json.Marshal(request.Options)
	if err != nil {
		return Decision{}, err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO decisions(project_id,task_id,origin,question,options_json,created_at)
		SELECT ?,?,?,?,?,? WHERE EXISTS (SELECT 1 FROM tasks WHERE id=? AND project_id=?)
		ON CONFLICT(project_id,origin) DO NOTHING`, request.ProjectID, request.TaskID, request.Origin, request.Question, string(options), time.Now().UnixMilli(), request.TaskID, request.ProjectID)
	if err != nil {
		return Decision{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Decision{}, err
	}
	decision, err := decisionRow(tx.QueryRowContext(ctx, `SELECT id,project_id,task_id,origin,question,options_json,answer,user_quote,created_at,answered_at FROM decisions WHERE project_id=? AND origin=?`, request.ProjectID, request.Origin))
	if errors.Is(err, ErrNotFound) {
		return Decision{}, fmt.Errorf("Decision Task does not belong to Project")
	}
	if err != nil {
		return Decision{}, err
	}
	if count == 0 && (decision.TaskID != request.TaskID || decision.Question != request.Question || !slices.Equal(request.Options, decision.Options)) {
		return Decision{}, fmt.Errorf("Decision origin %q already belongs to another question", request.Origin)
	}
	return decision, tx.Commit()
}

// AnswerDecision records one answer and one Notice atomically. A second answer
// cannot overwrite either the answer or the User's words.
func (db *DB) AnswerDecision(ctx context.Context, projectID, id int64, option, userQuote string) (Decision, error) {
	if strings.TrimSpace(userQuote) == "" {
		return Decision{}, fmt.Errorf("User quote is required")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer tx.Rollback()
	decision, err := decisionRow(tx.QueryRowContext(ctx, `SELECT id,project_id,task_id,origin,question,options_json,answer,user_quote,created_at,answered_at FROM decisions WHERE project_id=? AND id=?`, projectID, id))
	if err != nil {
		return Decision{}, err
	}
	if decision.AnsweredAt != 0 {
		return Decision{}, fmt.Errorf("Decision %d is already answered", id)
	}
	valid := false
	for _, choice := range decision.Options {
		valid = valid || choice == option
	}
	if !valid {
		return Decision{}, fmt.Errorf("%q is not a Decision option", option)
	}
	at := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE decisions SET answer=?,user_quote=?,answered_at=? WHERE id=? AND answered_at=0`, option, userQuote, at, id); err != nil {
		return Decision{}, err
	}
	data, err := json.Marshal(map[string]any{"decision_id": id, "option": option, "user_quote": userQuote})
	if err != nil {
		return Decision{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,'decision_answered',?,?,?)`, projectID, decision.TaskID, fmt.Sprintf("Decision #%d answered: %s. Carry out the chosen action", id, option), string(data), at); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(); err != nil {
		return Decision{}, err
	}
	decision.Answer, decision.UserQuote, decision.AnsweredAt = option, userQuote, at
	return decision, nil
}

func (db *DB) Decision(ctx context.Context, projectID, id int64) (Decision, error) {
	return decisionRow(db.QueryRowContext(ctx, `SELECT id,project_id,task_id,origin,question,options_json,answer,user_quote,created_at,answered_at FROM decisions WHERE project_id=? AND id=?`, projectID, id))
}

// DecisionSourceNotices returns only Notices not yet represented by a Decision.
func (db *DB) DecisionSourceNotices(ctx context.Context, projectID int64) ([]Notice, error) {
	rows, err := db.QueryContext(ctx, `SELECT n.id,n.project_id,COALESCE(n.task_id,0),n.kind,n.summary,n.data_json,n.created_at,COALESCE(n.delivered_at,0),COALESCE(n.acked_at,0)
		FROM notices n LEFT JOIN decisions d ON d.project_id=n.project_id AND d.origin='notice:' || n.id
		WHERE n.project_id=? AND d.id IS NULL AND n.kind IN ('land_ready','task_failed','task_lost','task_done','needs_decision') ORDER BY n.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Notice{}
	for rows.Next() {
		var n Notice
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.TaskID, &n.Kind, &n.Summary, &n.DataJSON, &n.CreatedAt, &n.DeliveredAt, &n.AckedAt); err != nil {
			return nil, err
		}
		result = append(result, n)
	}
	return result, rows.Err()
}

func (db *DB) Decisions(ctx context.Context, projectID int64, pendingOnly bool) ([]Decision, error) {
	query := `SELECT id,project_id,task_id,origin,question,options_json,answer,user_quote,created_at,answered_at FROM decisions WHERE project_id=?`
	if pendingOnly {
		query += ` AND answered_at=0`
	}
	rows, err := db.QueryContext(ctx, query+` ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	decisions := []Decision{}
	for rows.Next() {
		decision, err := decisionRow(rows)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

type decisionScanner interface{ Scan(...any) error }

func decisionRow(row decisionScanner) (Decision, error) {
	var decision Decision
	var options string
	err := row.Scan(&decision.ID, &decision.ProjectID, &decision.TaskID, &decision.Origin, &decision.Question, &options, &decision.Answer, &decision.UserQuote, &decision.CreatedAt, &decision.AnsweredAt)
	if err == nil {
		err = json.Unmarshal([]byte(options), &decision.Options)
	}
	return decision, err
}
