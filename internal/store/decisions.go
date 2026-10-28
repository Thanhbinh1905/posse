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
	if request.ProjectID < 1 || request.TaskID < 1 || strings.TrimSpace(request.Origin) == "" || strings.TrimSpace(request.Question) == "" || len(request.Options) < 1 {
		return Decision{}, fmt.Errorf("decision requires a Project, Task, origin, question and at least one option")
	}
	switch request.Kind {
	case "merge_ready", "recovery", "worker_exited", "review", "review_cap", "worker_question", "orphan", "pr_closed", "dependency", "quota":
	default:
		return Decision{}, fmt.Errorf("invalid Decision kind %q", request.Kind)
	}
	seen := map[string]bool{}
	for _, option := range request.Options {
		if strings.TrimSpace(option) != option || option == "" || seen[option] {
			return Decision{}, fmt.Errorf("decision options must be distinct and non-empty")
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
		// A dependency question names the prerequisite's current state, which
		// may move (failed, then torn-down) under the same origin.
		if original.Kind == "dependency" && request.Kind == "dependency" && original.TaskID == request.TaskID && original.Question != request.Question && slices.Equal(original.Options, request.Options) {
			if original.AnsweredAt != 0 || original.ObsoleteAt != 0 {
				return original, nil
			}
			if _, err := tx.ExecContext(ctx, `UPDATE decisions SET question=? WHERE id=?`, request.Question, original.ID); err != nil {
				return Decision{}, err
			}
			if err := tx.Commit(); err != nil {
				return Decision{}, err
			}
			if err := db.PersistProject(ctx, request.ProjectID); err != nil {
				return Decision{}, err
			}
			original.Question = request.Question
			return original, nil
		}
		if original.TaskID != request.TaskID || original.Kind != request.Kind || original.Question != request.Question || !slices.Equal(original.Options, request.Options) {
			return Decision{}, fmt.Errorf("decision origin %q already belongs to another question", request.Origin)
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
		return Decision{}, fmt.Errorf("decision Task does not belong to Project")
	}
	if err != nil {
		return Decision{}, err
	}
	if count == 0 && (decision.TaskID != request.TaskID || decision.Kind != request.Kind || decision.Question != request.Question || !slices.Equal(request.Options, decision.Options)) {
		return Decision{}, fmt.Errorf("decision origin %q already belongs to another question", request.Origin)
	}
	if err := tx.Commit(); err != nil {
		return Decision{}, err
	}
	if err := db.PersistProject(ctx, request.ProjectID); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

// ObsoleteStaleReviewCapDecisions retires unanswered cap choices for an older
// exact head, preserving answered history.
func (db *DB) ObsoleteStaleReviewCapDecisions(ctx context.Context, taskID int64, currentOrigin string) error {
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=?`, taskID).Scan(&projectID); err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason='Review candidate head changed'
WHERE task_id=? AND kind='review_cap' AND answered_at=0 AND obsolete_at=0 AND origin<>?`, time.Now().UnixMilli(), taskID, currentOrigin)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

// ObsoletePendingPRDecision retires a stale observation without changing a
// User's answer or affecting a different Member's PR.
func (db *DB) ObsoletePendingPRDecision(ctx context.Context, taskID int64, origin, reason string) error {
	result, err := db.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?, obsolete_reason=? WHERE task_id=? AND kind='pr_closed' AND origin=? AND answered_at=0 AND obsolete_at=0`, time.Now().UnixMilli(), reason, taskID, origin)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=?`, taskID).Scan(&projectID); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func (db *DB) ObsoletePendingOrphanDecision(ctx context.Context, id int64) error {
	result, err := db.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason='Orphan has no content diff' WHERE id=? AND kind='orphan' AND answered_at=0 AND obsolete_at=0`, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM decisions WHERE id=?`, id).Scan(&projectID); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

// ObsoleteAppliedOrphanDecision retires an unrecoverable Orphan Decision
// after its approved discard has completed Teardown.
func (db *DB) ObsoleteAppliedOrphanDecision(ctx context.Context, id int64) error {
	result, err := db.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason='Unrecoverable Orphan resolved during Teardown'
		WHERE id=? AND kind='orphan' AND origin LIKE 'orphan:unrecoverable:%' AND answer='discard' AND answered_at>0 AND obsolete_at=0
		AND EXISTS(SELECT 1 FROM tasks WHERE tasks.id=decisions.task_id AND tasks.state='torn-down')`, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM decisions WHERE id=?`, id).Scan(&projectID); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

// QueueDecisionMessage makes applying a Review answer retry-safe.
func (db *DB) QueueDecisionMessage(ctx context.Context, decision Decision, targetID int64, body string) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key := fmt.Sprintf("%d", decision.ID)
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE project_id=? AND kind='decision_applied' AND data_json=?)`, decision.ProjectID, key).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,status,wait_for_idle) VALUES(?,?,?,'queued',1)`, targetID, body, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at,delivered_at,acked_at) VALUES(?,?,'decision_applied',?,?,?, ?, ?)`, decision.ProjectID, decision.TaskID, "Review answer applied", key, now, now, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistProject(ctx, decision.ProjectID)
}

// AnswerDecision records one answer and one Notice atomically. A second answer
// cannot overwrite either the answer or the User's words.
func (db *DB) AnswerDecision(ctx context.Context, projectID, id int64, option, userQuote string) (Decision, error) {
	if strings.TrimSpace(userQuote) == "" {
		return Decision{}, fmt.Errorf("user quote is required")
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
		return Decision{}, fmt.Errorf("decision %d is already answered", id)
	}
	if decision.ObsoleteAt != 0 {
		return Decision{}, fmt.Errorf("decision %d is obsolete: %s", id, decision.ObsoleteReason)
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
		if err := db.PersistProject(ctx, projectID); err != nil {
			return Decision{}, err
		}
		return Decision{}, fmt.Errorf("decision %d is obsolete: %s", id, reason)
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
	if err := db.PersistProject(ctx, projectID); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func (db *DB) Decision(ctx context.Context, projectID, id int64) (Decision, error) {
	return decisionRow(db.QueryRowContext(ctx, decisionSelect+` WHERE project_id=? AND id=?`, projectID, id))
}

const reviewCapNoticesQuery = `SELECT n.id,n.project_id,COALESCE(n.task_id,0),n.kind,n.summary,n.data_json,n.created_at,COALESCE(n.delivered_at,0),COALESCE(n.acked_at,0)
		FROM tasks t INDEXED BY tasks_project_state_idx JOIN notices n INDEXED BY notices_task_kind_idx ON n.task_id=t.id AND n.kind='review_cap_reached'
		WHERE t.project_id=? AND n.project_id=t.project_id AND t.type='change' AND t.state<>'torn-down'
		ORDER BY n.id`

// ReviewCapNotices returns cap Notices regardless of the general decision cursor,
// so a newer exact-head cap cannot be skipped by older reconciliation state.
// Torn-down Tasks are excluded, so the scan does not grow with Project history.
func (db *DB) ReviewCapNotices(ctx context.Context, projectID int64) ([]Notice, error) {
	rows, err := db.QueryContext(ctx, reviewCapNoticesQuery, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []Notice
	for rows.Next() {
		var notice Notice
		if err := rows.Scan(&notice.ID, &notice.ProjectID, &notice.TaskID, &notice.Kind, &notice.Summary, &notice.DataJSON, &notice.CreatedAt, &notice.DeliveredAt, &notice.AckedAt); err != nil {
			return nil, err
		}
		notices = append(notices, notice)
	}
	return notices, rows.Err()
}

const decisionSourceNoticesQuery = `SELECT n.id,n.project_id,COALESCE(n.task_id,0),n.kind,n.summary,n.data_json,n.created_at,COALESCE(n.delivered_at,0),COALESCE(n.acked_at,0)
		FROM notices n WHERE n.project_id=? AND n.id>COALESCE((SELECT last_notice_id FROM decision_notice_cursors WHERE project_id=?),0)
		UNION
		SELECT n.id,n.project_id,COALESCE(n.task_id,0),n.kind,n.summary,n.data_json,n.created_at,COALESCE(n.delivered_at,0),COALESCE(n.acked_at,0)
		FROM tasks t INDEXED BY tasks_project_state_idx JOIN notices n INDEXED BY notices_task_kind_idx ON n.task_id=t.id AND n.kind='merge_ready'
		WHERE t.project_id=? AND n.project_id=t.project_id AND t.state IN ('done','merging') AND t.autonomy_land<>'auto'
		AND NOT EXISTS(SELECT 1 FROM decisions d WHERE d.project_id=n.project_id AND d.origin='notice:'||n.id)
		ORDER BY 1`

// DecisionSourceNotices returns unevaluated Notices and merge_ready Notices
// without a Decision, even if the cursor advanced before reconciliation completed.
// Each half reads its own index range: Notices past the cursor, and the
// merge_ready Notices of merge-ready Tasks, never the Project's whole history.
func (db *DB) DecisionSourceNotices(ctx context.Context, projectID int64) ([]Notice, error) {
	rows, err := db.QueryContext(ctx, decisionSourceNoticesQuery, projectID, projectID, projectID)
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

const unansweredWorkerExitedNoticesQuery = `SELECT n.id,n.project_id,COALESCE(n.task_id,0),n.kind,n.summary,n.data_json,n.created_at,COALESCE(n.delivered_at,0),COALESCE(n.acked_at,0)
		FROM tasks t INDEXED BY tasks_project_state_idx JOIN notices n INDEXED BY notices_task_kind_idx ON n.task_id=t.id AND n.kind='worker_exited'
		WHERE t.project_id=? AND n.project_id=t.project_id AND t.state IN ('done','merging')
		AND (json_extract(n.data_json,'$.task_launches')=t.launches OR (json_type(n.data_json,'$.task_launches') IS NULL AND t.launches=1))
		AND NOT EXISTS(SELECT 1 FROM decisions d WHERE d.project_id=n.project_id AND d.origin='notice:'||n.id)
		ORDER BY n.id`

// UnansweredWorkerExitedNotices returns completion Notices skipped by an older
// decision cursor, unless their Task has since reopened or a Decision exists.
func (db *DB) UnansweredWorkerExitedNotices(ctx context.Context, projectID int64) ([]Notice, error) {
	rows, err := db.QueryContext(ctx, unansweredWorkerExitedNoticesQuery, projectID)
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
	if _, err := db.ExecContext(ctx, `INSERT INTO decision_notice_cursors(project_id,last_notice_id) VALUES(?,?)
		ON CONFLICT(project_id) DO UPDATE SET last_notice_id=MAX(last_notice_id,excluded.last_notice_id)`, projectID, id); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

// ObsoleteResolvedDecisions removes unanswered questions whose situation has
// resolved. Answered Decisions are otherwise retained.
func (db *DB) ObsoleteResolvedDecisions(ctx context.Context, projectID int64) error {
	result, err := db.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason=CASE kind
		WHEN 'merge_ready' THEN CASE WHEN EXISTS(SELECT 1 FROM tasks WHERE id=decisions.task_id AND state NOT IN ('done','merging')) THEN 'Task left merge-ready state' ELSE 'PR is no longer ready to merge' END
		WHEN 'worker_exited' THEN 'Task reopened or Worker launch changed'
		WHEN 'pr_closed' THEN 'Closed PR was reopened, replaced or Task discarded'
		WHEN 'orphan' THEN 'Unrecoverable Orphan resolved during Teardown'
		WHEN 'quota' THEN 'Worker relaunched or Task left its working states'
		WHEN 'dependency' THEN 'Task is no longer queued'
		ELSE 'Task no longer failed or lost' END
		WHERE project_id=? AND answered_at=0 AND obsolete_at=0 AND kind IN ('merge_ready','recovery','worker_exited','pr_closed','orphan','quota','dependency')
		AND EXISTS(SELECT 1 FROM tasks t WHERE t.id=decisions.task_id AND
		((kind='merge_ready' AND (t.state NOT IN ('done','merging') OR (t.landing_mode='pr' AND NOT EXISTS(SELECT 1 FROM pr_observations p WHERE p.task_id=t.id AND p.pr_url=t.pr_url AND p.id=(SELECT MAX(id) FROM pr_observations WHERE task_id=t.id AND pr_url=t.pr_url) AND p.state='OPEN' AND p.head_sha=t.gated_sha AND json_extract(p.checks,'$.state')='SUCCESS' AND p.review NOT IN ('CHANGES_REQUESTED','REVIEW_REQUIRED') AND p.mergeable='MERGEABLE'))))
		OR (kind='recovery' AND (t.state NOT IN ('failed','lost') OR t.launches<>decisions.task_launches))
		OR (kind='worker_exited' AND (t.state NOT IN ('done','merging') OR t.launches<>decisions.task_launches))
		OR (kind='dependency' AND t.state<>'queued')
		OR (kind='quota' AND (t.state NOT IN ('working','stalled','blocked','needs-decision') OR t.launches<>decisions.task_launches))
		OR (kind='pr_closed' AND ((decisions.origin LIKE 'pr_closed:invalid:%' AND (t.state='torn-down' OR decisions.origin<>'pr_closed:invalid:'||t.id||':'||t.pr_url)) OR (decisions.origin NOT LIKE 'pr_closed:invalid:%' AND (t.state='torn-down' OR (t.pr_url<>substr(decisions.origin,11) AND NOT EXISTS(SELECT 1 FROM task_repos r WHERE r.task_id=t.id AND r.pr_url=substr(decisions.origin,11))) OR (SELECT state FROM pr_observations WHERE task_id=t.id AND pr_url=substr(decisions.origin,11) ORDER BY id DESC LIMIT 1)<>'CLOSED'))))
		OR (kind='orphan' AND decisions.origin LIKE 'orphan:unrecoverable:%' AND t.state='torn-down')))`, time.Now().UnixMilli(), projectID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

func decisionObsoleteReason(ctx context.Context, tx *writeTx, decision Decision) (string, error) {
	if decision.Kind != "merge_ready" && decision.Kind != "recovery" && decision.Kind != "worker_exited" && decision.Kind != "pr_closed" && decision.Kind != "orphan" && decision.Kind != "review_cap" && decision.Kind != "quota" {
		return "", nil
	}
	var state State
	var launches int
	if err := tx.QueryRowContext(ctx, `SELECT state,launches FROM tasks WHERE id=? AND project_id=?`, decision.TaskID, decision.ProjectID).Scan(&state, &launches); err != nil {
		return "", err
	}
	if decision.Kind == "merge_ready" {
		var ready bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks t WHERE t.id=? AND t.state IN ('done','merging') AND (t.landing_mode<>'pr' OR EXISTS(SELECT 1 FROM pr_observations p WHERE p.task_id=t.id AND p.pr_url=t.pr_url AND p.id=(SELECT MAX(id) FROM pr_observations WHERE task_id=t.id AND pr_url=t.pr_url) AND p.state='OPEN' AND p.head_sha=t.gated_sha AND json_extract(p.checks,'$.state')='SUCCESS' AND p.review NOT IN ('CHANGES_REQUESTED','REVIEW_REQUIRED') AND p.mergeable='MERGEABLE')))`, decision.TaskID).Scan(&ready); err != nil {
			return "", err
		}
		if state != StateDone && state != StateMerging {
			return "Task left merge-ready state", nil
		}
		if !ready {
			return "PR is no longer ready to merge", nil
		}
	}
	if decision.Kind == "recovery" && (state != StateFailed && state != StateLost || launches != decision.TaskLaunches) {
		return "Task no longer failed or lost", nil
	}
	if decision.Kind == "worker_exited" && (state != StateDone && state != StateMerging || launches != decision.TaskLaunches) {
		return "Task reopened or Worker launch changed", nil
	}
	if decision.Kind == "quota" && (!QuotaDecisionState(state) || launches != decision.TaskLaunches) {
		return "Worker relaunched or Task left its working states", nil
	}
	if decision.Kind == "pr_closed" {
		var owned bool
		var latest string
		url := strings.TrimPrefix(decision.Origin, "pr_closed:")
		invalid := strings.HasPrefix(url, "invalid:")
		if invalid {
			_, url, _ = strings.Cut(strings.TrimPrefix(url, "invalid:"), ":")
		}
		if err := tx.QueryRowContext(ctx, `SELECT (t.pr_url=? OR EXISTS(SELECT 1 FROM task_repos r WHERE r.task_id=t.id AND r.pr_url=?)), COALESCE((SELECT state FROM pr_observations WHERE task_id=t.id AND pr_url=? ORDER BY id DESC LIMIT 1),'') FROM tasks t WHERE t.id=?`, url, url, url, decision.TaskID).Scan(&owned, &latest); err != nil {
			return "", err
		}
		if state == StateTornDown || !owned || (latest != "CLOSED" && !invalid) {
			return "Closed PR was reopened, replaced or Task discarded", nil
		}
	}
	if decision.Kind == "orphan" && strings.HasPrefix(decision.Origin, "orphan:unrecoverable:") && state == StateTornDown {
		return "Unrecoverable Orphan resolved during Teardown", nil
	}
	return "", nil
}

func (db *DB) Decisions(ctx context.Context, projectID int64, pendingOnly bool) ([]Decision, error) {
	if err := db.ObsoleteResolvedDecisions(ctx, projectID); err != nil {
		return nil, err
	}
	return db.readDecisions(ctx, projectID, pendingOnly)
}

// PendingDecisions reads unanswered Decisions without obsoleting resolved
// ones first, so it also works on a read-only connection.
func (db *DB) PendingDecisions(ctx context.Context, projectID int64) ([]Decision, error) {
	return db.readDecisions(ctx, projectID, true)
}

func (db *DB) readDecisions(ctx context.Context, projectID int64, pendingOnly bool) ([]Decision, error) {
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
