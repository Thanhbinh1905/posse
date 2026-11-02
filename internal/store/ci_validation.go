package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CIRerunCheck identifies one provider check covered by a single rerun action.
type CIRerunCheck struct {
	Name  string
	RunID string
	JobID string
	URL   string
}

// ErrStaleValidationHead means a CI instruction no longer matches the Task's gated PR head.
var ErrStaleValidationHead = errors.New("CI observation is not for the current gated head")

// ErrValidationFixBudgetExhausted means the per-Task automatic fix-round budget is spent.
var ErrValidationFixBudgetExhausted = errors.New("validation fix-round budget exhausted")

// ReserveCIRerunRequests records retry attempts before invoking the forge. The
// insert is conditional on the Task still being gated at headSHA, so stale
// observations cannot consume budget or request a rerun.
func (db *DB) ReserveCIRerunRequests(ctx context.Context, projectID, taskID int64, member, prURL, headSHA, provider string, checks []CIRerunCheck, maxAttempts int) (bool, error) {
	if projectID < 1 || taskID < 1 || strings.TrimSpace(prURL) == "" || !validCommitSHA(headSHA) || (provider != "github" && provider != "gitlab") || maxAttempts < 0 {
		return false, fmt.Errorf("invalid CI rerun reservation")
	}
	if maxAttempts == 0 || len(checks) == 0 {
		return false, nil
	}
	unique := make([]CIRerunCheck, 0, len(checks))
	seen := map[string]bool{}
	for _, check := range checks {
		check.Name = strings.TrimSpace(check.Name)
		if check.Name == "" || strings.TrimSpace(check.RunID) == "" || (provider == "gitlab" && strings.TrimSpace(check.JobID) == "") {
			return false, fmt.Errorf("CI rerun check requires a name and provider retry ID")
		}
		if !seen[check.Name] {
			seen[check.Name] = true
			unique = append(unique, check)
		}
	}

	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var state, taskType, mergeMode, gatedSHA, currentPR string
	query := `SELECT state,type,landing_mode,gated_sha,pr_url FROM tasks WHERE id=? AND project_id=?`
	args := []any{taskID, projectID}
	if member != "" {
		query = `SELECT t.state,t.type,r.landing_mode,r.gated_sha,r.pr_url FROM tasks t JOIN task_repos r ON r.task_id=t.id WHERE t.id=? AND t.project_id=? AND r.repo=?`
		args = append(args, member)
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&state, &taskType, &mergeMode, &gatedSHA, &currentPR); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if taskType != "change" || mergeMode != "pr" || currentPR != prURL || gatedSHA != headSHA || (state != string(StateDone) && state != string(StateMerging)) {
		return false, nil
	}

	type attempt struct {
		check CIRerunCheck
		seq   int
	}
	attempts := make([]attempt, 0, len(unique))
	for _, check := range unique {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE project_id=? AND task_id=? AND kind='ci_rerun_requested' AND json_valid(data_json) AND json_extract(data_json,'$.head_sha')=? AND json_extract(data_json,'$.check_name')=? AND COALESCE(json_extract(data_json,'$.repository'),'')=?`, projectID, taskID, headSHA, check.Name, member).Scan(&count); err != nil {
			return false, err
		}
		if count >= maxAttempts {
			return false, nil
		}
		attempts = append(attempts, attempt{check: check, seq: count + 1})
	}

	now := time.Now().UnixMilli()
	for _, item := range attempts {
		data, err := json.Marshal(struct {
			HeadSHA    string `json:"head_sha"`
			CheckName  string `json:"check_name"`
			Provider   string `json:"provider"`
			RunID      string `json:"run_id"`
			JobID      string `json:"job_id"`
			URL        string `json:"url"`
			Repository string `json:"repository"`
			Attempt    int    `json:"attempt"`
		}{headSHA, item.check.Name, provider, item.check.RunID, item.check.JobID, item.check.URL, member, item.seq})
		if err != nil {
			return false, err
		}
		memberLabel := ""
		if member != "" {
			memberLabel = " for workspace member " + member
		}
		summary := fmt.Sprintf("Retrying provider-cancelled CI check %s%s on gated head %s (attempt %d of %d)", item.check.Name, memberLabel, headSHA, item.seq, maxAttempts)
		result, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at)
			SELECT ?,?,'ci_rerun_requested',?,?,?
			WHERE NOT EXISTS (SELECT 1 FROM notices WHERE project_id=? AND task_id=? AND kind='ci_rerun_requested' AND json_valid(data_json) AND json_extract(data_json,'$.head_sha')=? AND json_extract(data_json,'$.check_name')=? AND COALESCE(json_extract(data_json,'$.repository'),'')=? AND json_extract(data_json,'$.attempt')=?)`,
			projectID, taskID, summary, string(data), now, projectID, taskID, headSHA, item.check.Name, member, item.seq)
		if err != nil {
			return false, err
		}
		if inserted, err := result.RowsAffected(); err != nil {
			return false, err
		} else if inserted != 1 {
			return false, nil
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, db.PersistProject(ctx, projectID)
}

// QueueCIFixInstruction records a CI fix round if the gated PR head and frozen
// per-Task budget still match, and queues or extends its Worker message atomically.
func (db *DB) QueueCIFixInstruction(ctx context.Context, taskID int64, expectedBaseHead, prURL, dedupeKey, body, member string, maxFixRounds int) (QueueValidationFixResult, error) {
	if taskID < 1 || !validCommitSHA(expectedBaseHead) || strings.TrimSpace(prURL) == "" || strings.TrimSpace(dedupeKey) == "" || strings.TrimSpace(body) == "" || maxFixRounds < 0 {
		return QueueValidationFixResult{}, fmt.Errorf("invalid CI fix instruction or budget")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return QueueValidationFixResult{}, err
	}
	defer tx.Rollback()

	var state, taskType, mergeMode, gatedSHA, currentPR string
	if err := tx.QueryRowContext(ctx, `SELECT state,type,landing_mode,gated_sha,pr_url FROM tasks WHERE id=?`, taskID).Scan(&state, &taskType, &mergeMode, &gatedSHA, &currentPR); err != nil {
		return QueueValidationFixResult{}, err
	}
	if taskType != "change" || !taskAcceptsInstruction(State(state), taskType) {
		return QueueValidationFixResult{}, ErrStateRace
	}
	if member != "" {
		if err := tx.QueryRowContext(ctx, `SELECT landing_mode,gated_sha,pr_url FROM task_repos WHERE task_id=? AND repo=?`, taskID, member).Scan(&mergeMode, &gatedSHA, &currentPR); err != nil {
			return QueueValidationFixResult{}, ErrStaleValidationHead
		}
	}
	if mergeMode != "pr" || currentPR != prURL || gatedSHA != expectedBaseHead {
		return QueueValidationFixResult{}, ErrStaleValidationHead
	}

	var existingMessageID int64
	var existingExpected string
	err = tx.QueryRowContext(ctx, `SELECT expected_base_head,message_id FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci' AND dedupe_key=?`, taskID, dedupeKey).Scan(&existingExpected, &existingMessageID)
	if err == nil {
		if existingExpected != expectedBaseHead {
			return QueueValidationFixResult{}, fmt.Errorf("CI fix dedupe key %q was reused for another head", dedupeKey)
		}
		return QueueValidationFixResult{MessageID: existingMessageID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return QueueValidationFixResult{}, err
	}
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, taskID).Scan(&used); err != nil {
		return QueueValidationFixResult{}, err
	}
	if used >= maxFixRounds {
		return QueueValidationFixResult{}, ErrValidationFixBudgetExhausted
	}

	var seq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM validation_fix_rounds WHERE ship_task_id=? AND kind='ci'`, taskID).Scan(&seq); err != nil {
		return QueueValidationFixResult{}, err
	}
	var messageID int64
	var coalesced bool
	var existingBody string
	err = tx.QueryRowContext(ctx, `SELECT m.id,m.body FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id WHERE r.ship_task_id=? AND r.expected_base_head=? AND m.status='queued' ORDER BY m.id LIMIT 1`, taskID, expectedBaseHead).Scan(&messageID, &existingBody)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return QueueValidationFixResult{}, err
	}
	createdAt := time.Now().UnixMilli()
	var lastDone sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(at) FROM signals WHERE task_id=? AND verb='done'`, taskID).Scan(&lastDone); err != nil {
		return QueueValidationFixResult{}, err
	}
	if lastDone.Valid && createdAt <= lastDone.Int64 {
		createdAt = lastDone.Int64 + 1
	}
	if messageID != 0 {
		updated, err := tx.ExecContext(ctx, `UPDATE messages SET body=? WHERE id=? AND status='queued'`, existingBody+"\n\n---\n\n"+strings.TrimSpace(body), messageID)
		if err != nil {
			return QueueValidationFixResult{}, err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return QueueValidationFixResult{}, err
		}
		if changed != 1 {
			return QueueValidationFixResult{}, ErrStateRace
		}
		coalesced = true
	} else {
		result, err := tx.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,status,wait_for_idle) VALUES(?,?,?,'queued',1)`, taskID, strings.TrimSpace(body), createdAt)
		if err != nil {
			return QueueValidationFixResult{}, err
		}
		messageID, err = result.LastInsertId()
		if err != nil {
			return QueueValidationFixResult{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO validation_fix_rounds(ship_task_id,kind,seq,expected_base_head,dedupe_key,message_id,created_at) VALUES(?,'ci',?,?,?,?,?)`, taskID, seq, expectedBaseHead, dedupeKey, messageID, createdAt)
	if err != nil {
		return QueueValidationFixResult{}, err
	}
	if _, err := result.LastInsertId(); err != nil {
		return QueueValidationFixResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return QueueValidationFixResult{}, err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return QueueValidationFixResult{}, err
	}
	return QueueValidationFixResult{MessageID: messageID, Queued: true, Coalesced: coalesced}, nil
}

// CreateValidationNoticeOnce deduplicates one CI outcome by head and failing
// check set while retaining detailed URLs and log tails in the Notice payload.
func (db *DB) CreateValidationNoticeOnce(ctx context.Context, notice Notice, prURL, headSHA, checkSet string, member ...string) (bool, error) {
	if notice.ProjectID < 1 || notice.TaskID < 1 || (notice.Kind != "ci_fix_queued" && notice.Kind != "ci_fix_exhausted") || strings.TrimSpace(prURL) == "" || !validCommitSHA(headSHA) || strings.TrimSpace(checkSet) == "" {
		return false, fmt.Errorf("invalid validation Notice identity")
	}
	if notice.DataJSON == "" {
		notice.DataJSON = "{}"
	}
	if !json.Valid([]byte(notice.DataJSON)) {
		return false, fmt.Errorf("validation Notice data is not valid JSON")
	}
	if len(member) > 1 {
		return false, fmt.Errorf("validation Notice accepts at most one workspace member")
	}
	memberName := ""
	if len(member) == 1 {
		memberName = member[0]
	}
	createdAt := notice.CreatedAt
	if createdAt == 0 {
		createdAt = time.Now().UnixMilli()
	}
	gateCheck := `EXISTS (SELECT 1 FROM tasks t WHERE t.id=? AND t.project_id=? AND t.type='change' AND t.landing_mode='pr' AND t.pr_url=? AND t.gated_sha=? AND t.state IN ('done','merging'))`
	gateArgs := []any{notice.TaskID, notice.ProjectID, prURL, headSHA}
	if memberName != "" {
		gateCheck = `EXISTS (SELECT 1 FROM tasks t JOIN task_repos r ON r.task_id=t.id WHERE t.id=? AND t.project_id=? AND t.type='change' AND t.state IN ('done','merging') AND r.repo=? AND r.landing_mode='pr' AND r.pr_url=? AND r.gated_sha=?)`
		gateArgs = []any{notice.TaskID, notice.ProjectID, memberName, prURL, headSHA}
	}
	if notice.Kind == "ci_fix_queued" {
		// The fix round is already recorded, and delivering its message may have
		// reopened the Task and cleared the gated head before this Notice lands.
		// The recorded round, not the Task's current state, proves the fix was queued.
		gateCheck = `EXISTS (SELECT 1 FROM validation_fix_rounds r WHERE r.ship_task_id=? AND r.kind='ci' AND r.expected_base_head=?)`
		gateArgs = []any{notice.TaskID, headSHA}
	}
	query := `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) SELECT ?,?,?,?,?,? WHERE ` + gateCheck + ` AND NOT EXISTS (SELECT 1 FROM notices WHERE project_id=? AND task_id=? AND kind=? AND json_valid(data_json) AND json_extract(data_json,'$.head_sha')=? AND json_extract(data_json,'$.check_set')=?)`
	args := []any{notice.ProjectID, notice.TaskID, notice.Kind, notice.Summary, notice.DataJSON, createdAt}
	args = append(args, gateArgs...)
	args = append(args, notice.ProjectID, notice.TaskID, notice.Kind, headSHA, checkSet)
	result, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 0 {
		return inserted > 0, err
	}
	return true, db.PersistProject(ctx, notice.ProjectID)
}

func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
