package store

import (
	"context"
	"database/sql"
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
	Kind      string
	Question  string
	Options   []string
}

type Decision struct {
	ID             int64    `json:"id"`
	ProjectID      int64    `json:"project_id"`
	TaskID         int64    `json:"task_id"`
	Origin         string   `json:"origin"`
	Question       string   `json:"question"`
	Options        []string `json:"options"`
	Answer         string   `json:"answer"`
	UserQuote      string   `json:"user_quote"`
	CreatedAt      int64    `json:"created_at"`
	AnsweredAt     int64    `json:"answered_at"`
	Kind           string   `json:"kind"`
	TaskLaunches   int      `json:"task_launches"`
	ObsoleteAt     int64    `json:"obsolete_at"`
	ObsoleteReason string   `json:"obsolete_reason"`
}

func (db *DB) RaiseDecision(ctx context.Context, request DecisionRequest) (Decision, error) {
	if request.ProjectID < 1 || request.TaskID < 1 || strings.TrimSpace(request.Origin) == "" || strings.TrimSpace(request.Question) == "" || len(request.Options) < 2 {
		return Decision{}, fmt.Errorf("Decision requires a Project, Task, origin, question and at least two options")
	}
	switch request.Kind {
	case "land_ready", "recovery", "review", "rider_question", "leftover", "pr_closed":
	default:
		return Decision{}, fmt.Errorf("invalid Decision kind %q", request.Kind)
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
	if original, err := decisionRow(tx.QueryRowContext(ctx, decisionSelect+` WHERE project_id=? AND origin=?`, request.ProjectID, request.Origin)); err == nil {
		if original.TaskID != request.TaskID || original.Kind != request.Kind || original.Question != request.Question || !slices.Equal(original.Options, request.Options) {
			return Decision{}, fmt.Errorf("Decision origin %q already belongs to another question", request.Origin)
		}
		return original, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Decision{}, err
	}
	if request.Kind == "recovery" {
		var existingID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM decisions WHERE task_id=? AND kind='recovery' AND answered_at=0 AND obsolete_at=0`, request.TaskID).Scan(&existingID)
		if err == nil {
			return decisionRow(tx.QueryRowContext(ctx, decisionSelect+` WHERE id=?`, existingID))
		}
		if !errors.Is(err, ErrNotFound) {
			return Decision{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO decisions(project_id,task_id,origin,question,options_json,created_at,kind,task_launches)
		SELECT ?,?,?,?,?,?,?,launches FROM tasks WHERE id=? AND project_id=?
		ON CONFLICT(project_id,origin) DO NOTHING`, request.ProjectID, request.TaskID, request.Origin, request.Question, string(options), time.Now().UnixMilli(), request.Kind, request.TaskID, request.ProjectID)
	if err != nil {
		return Decision{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Decision{}, err
	}
	decision, err := decisionRow(tx.QueryRowContext(ctx, decisionSelect+` WHERE project_id=? AND origin=?`, request.ProjectID, request.Origin))
	if errors.Is(err, ErrNotFound) {
		return Decision{}, fmt.Errorf("Decision Task does not belong to Project")
	}
	if err != nil {
		return Decision{}, err
	}
	if count == 0 && (decision.TaskID != request.TaskID || decision.Kind != request.Kind || decision.Question != request.Question || !slices.Equal(request.Options, decision.Options)) {
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
	decision, err := decisionRow(tx.QueryRowContext(ctx, decisionSelect+` WHERE project_id=? AND id=?`, projectID, id))
	if err != nil {
		return Decision{}, err
	}
	if decision.AnsweredAt != 0 {
		return Decision{}, fmt.Errorf("Decision %d is already answered", id)
	}
	if decision.ObsoleteAt != 0 {
		return Decision{}, fmt.Errorf("Decision %d is obsolete: %s", id, decision.ObsoleteReason)
	}
	if reason, err := decisionObsoleteReason(ctx, tx, decision); err != nil {
		return Decision{}, err
	} else if reason != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason=? WHERE id=?`, time.Now().UnixMilli(), reason, id); err != nil {
			return Decision{}, err
		}
		if err := tx.Commit(); err != nil {
			return Decision{}, err
		}
		return Decision{}, fmt.Errorf("Decision %d is obsolete: %s", id, reason)
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
	return decisionRow(db.QueryRowContext(ctx, decisionSelect+` WHERE project_id=? AND id=?`, projectID, id))
}

// DecisionSourceNotices returns only Notices beyond the Project's evaluated cursor.
func (db *DB) DecisionSourceNotices(ctx context.Context, projectID int64) ([]Notice, error) {
	rows, err := db.QueryContext(ctx, `SELECT n.id,n.project_id,COALESCE(n.task_id,0),n.kind,n.summary,n.data_json,n.created_at,COALESCE(n.delivered_at,0),COALESCE(n.acked_at,0)
		FROM notices n WHERE n.project_id=? AND n.id>COALESCE((SELECT last_notice_id FROM decision_notice_cursors WHERE project_id=?),0)
		ORDER BY n.id`, projectID, projectID)
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

// AdvanceDecisionNoticeCursor marks all Notices up to id evaluated, including
// unrelated kinds and those that did not require a Decision. It is monotonic.
func (db *DB) AdvanceDecisionNoticeCursor(ctx context.Context, projectID, id int64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO decision_notice_cursors(project_id,last_notice_id) VALUES(?,?)
		ON CONFLICT(project_id) DO UPDATE SET last_notice_id=MAX(last_notice_id,excluded.last_notice_id)`, projectID, id)
	return err
}

// ObsoleteResolvedDecisions removes resolved questions from the pending list
// without pretending the User answered them. Answered and manual Decisions stay.
func (db *DB) ObsoleteResolvedDecisions(ctx context.Context, projectID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason=CASE kind
		WHEN 'land_ready' THEN 'Task left landing'
		ELSE 'Task no longer failed or lost' END
		WHERE project_id=? AND answered_at=0 AND obsolete_at=0 AND kind IN ('land_ready','recovery')
		AND EXISTS(SELECT 1 FROM tasks t WHERE t.id=decisions.task_id AND
		((kind='land_ready' AND t.state<>'landing') OR (kind='recovery' AND (t.state NOT IN ('failed','lost') OR t.launches<>decisions.task_launches))))`, time.Now().UnixMilli(), projectID)
	return err
}

func decisionObsoleteReason(ctx context.Context, tx *sql.Tx, decision Decision) (string, error) {
	if decision.Kind != "land_ready" && decision.Kind != "recovery" {
		return "", nil
	}
	var state State
	var launches int
	if err := tx.QueryRowContext(ctx, `SELECT state,launches FROM tasks WHERE id=? AND project_id=?`, decision.TaskID, decision.ProjectID).Scan(&state, &launches); err != nil {
		return "", err
	}
	if decision.Kind == "land_ready" && state != StateLanding {
		return "Task left landing", nil
	}
	if decision.Kind == "recovery" && (state != StateFailed && state != StateLost || launches != decision.TaskLaunches) {
		return "Task no longer failed or lost", nil
	}
	return "", nil
}

func (db *DB) Decisions(ctx context.Context, projectID int64, pendingOnly bool) ([]Decision, error) {
	if err := db.ObsoleteResolvedDecisions(ctx, projectID); err != nil {
		return nil, err
	}
	query := decisionSelect + ` WHERE project_id=?`
	if pendingOnly {
		query += ` AND answered_at=0 AND obsolete_at=0`
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

const decisionSelect = `SELECT id,project_id,task_id,origin,question,options_json,answer,user_quote,created_at,answered_at,kind,task_launches,obsolete_at,obsolete_reason FROM decisions`

type decisionScanner interface{ Scan(...any) error }

func decisionRow(row decisionScanner) (Decision, error) {
	var decision Decision
	var options string
	err := row.Scan(&decision.ID, &decision.ProjectID, &decision.TaskID, &decision.Origin, &decision.Question, &options, &decision.Answer, &decision.UserQuote, &decision.CreatedAt, &decision.AnsweredAt, &decision.Kind, &decision.TaskLaunches, &decision.ObsoleteAt, &decision.ObsoleteReason)
	if err == nil {
		err = json.Unmarshal([]byte(options), &decision.Options)
	}
	return decision, err
}
