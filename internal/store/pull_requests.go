package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type PRObservation struct {
	ID          int64
	ProjectID   int64
	TaskID      int64
	PRURL       string
	HeadSHA     string
	State       string
	Checks      string
	Review      string
	Mergeable   string
	MergeCommit string
	ObservedAt  int64
}

type PRTaskWatchFailure struct {
	ProjectID           int64
	TaskID              int64
	Repo                string
	PRURL               string
	ConsecutiveFailures int
	LastError           string
	UpdatedAt           int64
}

// PRPollClaimLease bounds how long an abandoned PR poll claim can suppress a later poll.
const PRPollClaimLease = 5 * time.Minute

// PRTaskWatchReminderInterval bounds how often an acknowledged PR failure can be reminded.
const PRTaskWatchReminderInterval = 24 * time.Hour

type ProjectWatchState struct {
	PRPolledAt            int64
	PRConsecutiveFailures int
	CheckoutCheckedAt     int64
	RootBehindHead        string
}

type MemberWatchError struct {
	Repo      string
	Phase     string
	Error     string
	UpdatedAt int64
}

type PRObservationEffect struct {
	Notices        []Notice
	TransitionTo   State
	TransitionNote string
	MergedRef      string
}

func (db *DB) RecordMemberWatchError(ctx context.Context, projectID int64, repo, phase, failure string, at int64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO member_watch_errors(project_id,repo,phase,error,updated_at) VALUES (?,?,?,?,?) ON CONFLICT(project_id,repo,phase) DO UPDATE SET error=excluded.error,updated_at=excluded.updated_at`, projectID, repo, phase, failure, at)
	return err
}

func (db *DB) ClearMemberWatchError(ctx context.Context, projectID int64, repo, phase string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM member_watch_errors WHERE project_id=? AND repo=? AND phase=?`, projectID, repo, phase)
	return err
}

func (db *DB) MemberWatchErrors(ctx context.Context, projectID int64) ([]MemberWatchError, error) {
	rows, err := db.QueryContext(ctx, `SELECT repo,phase,error,updated_at FROM member_watch_errors WHERE project_id=? ORDER BY repo,phase`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var failures []MemberWatchError
	for rows.Next() {
		var failure MemberWatchError
		if err := rows.Scan(&failure.Repo, &failure.Phase, &failure.Error, &failure.UpdatedAt); err != nil {
			return nil, err
		}
		failures = append(failures, failure)
	}
	return failures, rows.Err()
}

func (db *DB) ProjectWatchState(ctx context.Context, projectID int64) (ProjectWatchState, error) {
	var state ProjectWatchState
	err := db.QueryRowContext(ctx, `SELECT pr_polled_at, pr_consecutive_failures, checkout_checked_at, root_behind_head FROM project_watch_state WHERE project_id=?`, projectID).
		Scan(&state.PRPolledAt, &state.PRConsecutiveFailures, &state.CheckoutCheckedAt, &state.RootBehindHead)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectWatchState{}, nil
	}
	return state, err
}

// ClaimPRPoll reserves a Project poll before any forge call. SQLite serializes
// the conditional update across the Lead and Watch processes. The token
// prevents an old owner releasing a successor's lease after expiry.
func (db *DB) ClaimPRPoll(ctx context.Context, projectID int64, now time.Time, interval time.Duration, force bool) (string, error) {
	stamp := now.UnixMilli()
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes)
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO project_watch_state(project_id) VALUES (?)`, projectID); err != nil {
		return "", err
	}
	threshold := stamp - int64(interval/time.Millisecond)
	if force {
		threshold = math.MaxInt64
	}
	result, err := db.ExecContext(ctx, `UPDATE project_watch_state SET pr_poll_claim_until=?,pr_poll_claim_token=? WHERE project_id=? AND pr_poll_claim_until<=? AND pr_polled_at<=?`, stamp+int64(PRPollClaimLease/time.Millisecond), token, projectID, stamp, threshold)
	if err != nil {
		return "", err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if rows == 0 {
		return "", nil
	}
	return token, nil
}

// ClaimMemberPRPoll reserves one workspace Member independently, so another
// Member can be polled again while this Member's forge command remains stalled.
func (db *DB) ClaimMemberPRPoll(ctx context.Context, projectID int64, repo string, now time.Time, interval time.Duration, force bool) (string, error) {
	stamp := now.UnixMilli()
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes)
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO member_pr_poll_state(project_id,repo) VALUES (?,?)`, projectID, repo); err != nil {
		return "", err
	}
	threshold := stamp - int64(interval/time.Millisecond)
	if force {
		threshold = math.MaxInt64
	}
	result, err := db.ExecContext(ctx, `UPDATE member_pr_poll_state SET claim_until=?,claim_token=? WHERE project_id=? AND repo=? AND claim_until<=? AND pr_polled_at<=?`, stamp+int64(PRPollClaimLease/time.Millisecond), token, projectID, repo, stamp, threshold)
	if err != nil {
		return "", err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if rows == 0 {
		return "", nil
	}
	return token, nil
}

func (db *DB) RecordPRTaskWatchFailure(ctx context.Context, projectID, taskID int64, repo, prURL string, at int64, failure, summary string) (int, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if repo == "" {
		var currentPRURL string
		var state State
		if err := tx.QueryRowContext(ctx, `SELECT pr_url,state FROM tasks WHERE project_id=? AND id=?`, projectID, taskID).Scan(&currentPRURL, &state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, nil
			}
			return 0, err
		}
		if currentPRURL != prURL || state == StateMerged || state == StateTornDown || state == StateReported {
			return 0, nil
		}
	} else {
		var currentPRURL, repoState string
		var taskState State
		if err := tx.QueryRowContext(ctx, `SELECT r.pr_url,r.state,t.state FROM task_repos r JOIN tasks t ON t.id=r.task_id WHERE t.project_id=? AND r.task_id=? AND r.repo=?`, projectID, taskID, repo).Scan(&currentPRURL, &repoState, &taskState); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, nil
			}
			return 0, err
		}
		if currentPRURL != prURL || repoState == TaskRepoMerged || taskState == StateMerged || taskState == StateTornDown || taskState == StateReported {
			return 0, nil
		}
	}
	var previousPRURL string
	err = tx.QueryRowContext(ctx, `SELECT pr_url FROM pr_task_watch_failures WHERE project_id=? AND task_id=? AND repo=?`, projectID, taskID, repo).Scan(&previousPRURL)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	noticeChanged := false
	taskSnapshotChanged := previousPRURL != "" && previousPRURL != prURL
	if previousPRURL != "" && previousPRURL != prURL {
		result, err := tx.ExecContext(ctx, `UPDATE notices SET acked_at=COALESCE(acked_at,?) WHERE project_id=? AND task_id=? AND kind='pr_watch_failing' AND json_extract(data_json,'$.pr_url')=? AND COALESCE(json_extract(data_json,'$.repo'),'')=? AND acked_at IS NULL`, at, projectID, taskID, previousPRURL, repo)
		if err != nil {
			return 0, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		noticeChanged = rows > 0
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pr_task_watch_failures(project_id,task_id,repo,pr_url) VALUES (?,?,?,?)`, projectID, taskID, repo, prURL); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pr_task_watch_failures SET consecutive_failures=CASE WHEN pr_url<>? THEN 1 ELSE consecutive_failures+1 END,pr_url=?,last_error=?,updated_at=? WHERE project_id=? AND task_id=? AND repo=?`, prURL, prURL, failure, at, projectID, taskID, repo); err != nil {
		return 0, err
	}
	var failures int
	if err := tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM pr_task_watch_failures WHERE project_id=? AND task_id=? AND repo=?`, projectID, taskID, repo).Scan(&failures); err != nil {
		return 0, err
	}
	if failures == 1 {
		taskSnapshotChanged = true
	}
	if failures >= 3 {
		fields := map[string]any{"pr_url": prURL, "consecutive_failures": failures, "error": failure}
		if repo != "" {
			fields["repo"] = repo
		}
		var noticeID int64
		var existingSummary, existingData string
		var ackedAt sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT id,summary,data_json,acked_at FROM notices WHERE project_id=? AND task_id=? AND kind='pr_watch_failing' AND json_extract(data_json,'$.pr_url')=? AND COALESCE(json_extract(data_json,'$.repo'),'')=? AND id>COALESCE((SELECT MAX(id) FROM notices WHERE project_id=? AND task_id=? AND kind='pr_watch_recovered' AND json_extract(data_json,'$.pr_url')=? AND COALESCE(json_extract(data_json,'$.repo'),'')=?),0) ORDER BY id DESC LIMIT 1`, projectID, taskID, prURL, repo, projectID, taskID, prURL, repo).Scan(&noticeID, &existingSummary, &existingData, &ackedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if failures == 3 {
				data, err := json.Marshal(fields)
				if err != nil {
					return 0, err
				}
				if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, TaskID: taskID, Kind: "pr_watch_failing", Summary: summary, DataJSON: string(data), CreatedAt: at}); err != nil {
					return 0, err
				}
				noticeChanged = true
				taskSnapshotChanged = true
			}
		} else if ackedAt.Valid {
			var previous map[string]any
			_ = json.Unmarshal([]byte(existingData), &previous)
			reminderCount, _ := previous["reminder_count"].(float64)
			if at-ackedAt.Int64 >= int64(prTaskWatchReminderDelay(int(reminderCount))/time.Millisecond) {
				fields["reminder"] = true
				fields["reminder_count"] = int(reminderCount) + 1
				data, err := json.Marshal(fields)
				if err != nil {
					return 0, err
				}
				reminder := Notice{ProjectID: projectID, TaskID: taskID, Kind: "pr_watch_failing", Summary: "Persistent pull request polling failure reminder: " + failure, DataJSON: string(data), CreatedAt: at}
				if err := insertNoticeTx(ctx, tx, reminder); err != nil {
					return 0, err
				}
				noticeChanged = true
				taskSnapshotChanged = true
			}
		} else {
			var previous map[string]any
			_ = json.Unmarshal([]byte(existingData), &previous)
			if reminder, _ := previous["reminder"].(bool); reminder {
				fields["reminder"] = true
				fields["reminder_count"] = previous["reminder_count"]
				summary = "Persistent pull request polling failure reminder: " + failure
			}
			data, err := json.Marshal(fields)
			if err != nil {
				return 0, err
			}
			if existingSummary != summary || existingData != string(data) {
				if _, err := tx.ExecContext(ctx, `UPDATE notices SET summary=?,data_json=? WHERE id=?`, summary, string(data), noticeID); err != nil {
					return 0, err
				}
				noticeChanged = true
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if taskSnapshotChanged {
		if err := db.persistTask(ctx, taskID, false); err != nil {
			return 0, err
		}
	}
	if noticeChanged {
		if err := db.PersistProject(ctx, projectID); err != nil {
			return 0, err
		}
	}
	return failures, nil
}

func prTaskWatchReminderDelay(previousReminders int) time.Duration {
	switch previousReminders {
	case 0:
		return PRTaskWatchReminderInterval
	case 1:
		return 2 * PRTaskWatchReminderInterval
	default:
		return 7 * 24 * time.Hour
	}
}

func (db *DB) PRTaskWatchFailuresForTask(ctx context.Context, taskID int64) ([]PRTaskWatchFailure, error) {
	rows, err := db.QueryContext(ctx, `SELECT project_id,task_id,repo,pr_url,consecutive_failures,last_error,updated_at FROM pr_task_watch_failures WHERE task_id=? ORDER BY repo`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var failures []PRTaskWatchFailure
	for rows.Next() {
		var failure PRTaskWatchFailure
		if err := rows.Scan(&failure.ProjectID, &failure.TaskID, &failure.Repo, &failure.PRURL, &failure.ConsecutiveFailures, &failure.LastError, &failure.UpdatedAt); err != nil {
			return nil, err
		}
		failures = append(failures, failure)
	}
	return failures, rows.Err()
}

func (db *DB) FinishMemberPRPoll(ctx context.Context, projectID int64, repo, token string, polledAt int64) error {
	result, err := db.ExecContext(ctx, `UPDATE member_pr_poll_state SET pr_polled_at=?,claim_until=0,claim_token='' WHERE project_id=? AND repo=? AND claim_token=?`, polledAt, projectID, repo, token)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrStateRace
	}
	return nil
}

func (db *DB) ReleaseMemberPRPoll(ctx context.Context, projectID int64, repo, token string) error {
	_, err := db.ExecContext(ctx, `UPDATE member_pr_poll_state SET claim_until=0,claim_token='' WHERE project_id=? AND repo=? AND claim_token=?`, projectID, repo, token)
	return err
}

func (db *DB) ReleasePRPoll(ctx context.Context, projectID int64, token string) error {
	_, err := db.ExecContext(ctx, `UPDATE project_watch_state SET pr_poll_claim_until=0,pr_poll_claim_token='' WHERE project_id=? AND pr_poll_claim_token=?`, projectID, token)
	return err
}

func (db *DB) RecordPRPollAttempt(ctx context.Context, projectID int64, at int64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO project_watch_state(project_id,pr_polled_at) VALUES (?,?) ON CONFLICT(project_id) DO UPDATE SET pr_polled_at=excluded.pr_polled_at`, projectID, at)
	return err
}

func (db *DB) RecordPRPoll(ctx context.Context, projectID int64, at int64, failureSummary string) (int, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_watch_state(project_id) VALUES (?)`, projectID); err != nil {
		return 0, err
	}
	var previousFailures int
	if err := tx.QueryRowContext(ctx, `SELECT pr_consecutive_failures FROM project_watch_state WHERE project_id=?`, projectID).Scan(&previousFailures); err != nil {
		return 0, err
	}
	if failureSummary == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE project_watch_state SET pr_polled_at=?, pr_consecutive_failures=0 WHERE project_id=?`, at, projectID); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE project_watch_state SET pr_polled_at=?, pr_consecutive_failures=pr_consecutive_failures+1 WHERE project_id=?`, at, projectID); err != nil {
			return 0, err
		}
	}
	var failures int
	if err := tx.QueryRowContext(ctx, `SELECT pr_consecutive_failures FROM project_watch_state WHERE project_id=?`, projectID).Scan(&failures); err != nil {
		return 0, err
	}
	noticeChanged := false
	if failureSummary == "" && previousFailures >= 3 {
		data, err := json.Marshal(map[string]any{"consecutive_failures": previousFailures})
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notices SET acked_at=COALESCE(acked_at,?) WHERE project_id=? AND task_id IS NULL AND kind='pr_watch_failing'`, at, projectID); err != nil {
			return 0, err
		}
		if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, Kind: "pr_watch_recovered", Summary: "Pull request polling recovered", DataJSON: string(data), CreatedAt: at}); err != nil {
			return 0, err
		}
		noticeChanged = true
	} else if failureSummary != "" && failures >= 3 {
		data, err := json.Marshal(map[string]any{"consecutive_failures": failures, "error": failureSummary})
		if err != nil {
			return 0, err
		}
		summary := "Pull request polling failed three times: " + failureSummary
		var noticeID int64
		var existingSummary, existingData string
		err = tx.QueryRowContext(ctx, `SELECT id,summary,data_json FROM notices WHERE project_id=? AND task_id IS NULL AND kind='pr_watch_failing' AND id>COALESCE((SELECT MAX(id) FROM notices WHERE project_id=? AND task_id IS NULL AND kind='pr_watch_recovered'),0) ORDER BY id DESC LIMIT 1`, projectID, projectID).Scan(&noticeID, &existingSummary, &existingData)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if failures == 3 {
				if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, Kind: "pr_watch_failing", Summary: summary, DataJSON: string(data), CreatedAt: at}); err != nil {
					return 0, err
				}
				noticeChanged = true
			}
		} else if existingSummary != summary || existingData != string(data) {
			if _, err := tx.ExecContext(ctx, `UPDATE notices SET summary=?,data_json=? WHERE id=?`, summary, string(data), noticeID); err != nil {
				return 0, err
			}
			noticeChanged = true
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if noticeChanged {
		if err := db.PersistProject(ctx, projectID); err != nil {
			return 0, err
		}
	}
	return failures, nil
}

func (db *DB) RecordCheckoutAttempt(ctx context.Context, projectID int64, at int64) error {
	_, err := db.ExecContext(ctx, `INSERT INTO project_watch_state(project_id, checkout_checked_at) VALUES (?, ?) ON CONFLICT(project_id) DO UPDATE SET checkout_checked_at=excluded.checkout_checked_at`, projectID, at)
	return err
}

const repoFetchFailureNoticeAfter = 3

type repoFetchFailure struct {
	Repo                string `json:"repo"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	Error               string `json:"error"`
}

// RecordRepoFetchFailure increments one repository's fetch-failure streak and
// coalesces persistent failures into one Project-level Notice.
func (db *DB) RecordRepoFetchFailure(ctx context.Context, projectID int64, repo string, at int64, failure string) (int, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO repo_fetch_failures(project_id, repo) VALUES (?, ?)`, projectID, repo); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repo_fetch_failures SET consecutive_failures=consecutive_failures+1,last_error=?,updated_at=? WHERE project_id=? AND repo=?`, failure, at, projectID, repo); err != nil {
		return 0, err
	}
	var failures int
	if err := tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM repo_fetch_failures WHERE project_id=? AND repo=?`, projectID, repo).Scan(&failures); err != nil {
		return 0, err
	}
	changed, err := refreshRepoFetchFailureNoticeTx(ctx, tx, projectID, at, failures == repoFetchFailureNoticeAfter)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if changed {
		if err := db.PersistProject(ctx, projectID); err != nil {
			return 0, err
		}
	}
	return failures, nil
}

// RecordRepoFetchSuccess resets one repository's failure streak. The active
// Project Notice is acknowledged once no repository remains above the threshold,
// and one recovery Notice marks that transition.
func (db *DB) RecordRepoFetchSuccess(ctx context.Context, projectID int64, repo string, at int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	before, err := repoFetchFailuresTx(ctx, tx, projectID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repo_fetch_failures SET consecutive_failures=0,last_error='',updated_at=? WHERE project_id=? AND repo=?`, at, projectID, repo); err != nil {
		return err
	}
	changed, err := refreshRepoFetchFailureNoticeTx(ctx, tx, projectID, at, false)
	if err != nil {
		return err
	}
	remaining, err := repoFetchFailuresTx(ctx, tx, projectID)
	if err != nil {
		return err
	}
	if len(before) > 0 && len(remaining) == 0 {
		data, err := json.Marshal(map[string]any{"repositories": before})
		if err != nil {
			return err
		}
		var names []string
		for _, failure := range before {
			name := failure.Repo
			if name == "" {
				name = "Project"
			}
			names = append(names, name)
		}
		if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, Kind: "root_fetch_recovered", Summary: "Origin fetch recovered for " + strings.Join(names, ", "), DataJSON: string(data), CreatedAt: at}); err != nil {
			return err
		}
		changed = true
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed {
		return db.PersistProject(ctx, projectID)
	}
	return nil
}

func repoFetchFailuresTx(ctx context.Context, tx *writeTx, projectID int64) ([]repoFetchFailure, error) {
	rows, err := tx.QueryContext(ctx, `SELECT repo,consecutive_failures,last_error FROM repo_fetch_failures WHERE project_id=? AND consecutive_failures>=? ORDER BY repo`, projectID, repoFetchFailureNoticeAfter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var failures []repoFetchFailure
	for rows.Next() {
		var failure repoFetchFailure
		if err := rows.Scan(&failure.Repo, &failure.ConsecutiveFailures, &failure.Error); err != nil {
			return nil, err
		}
		failures = append(failures, failure)
	}
	return failures, rows.Err()
}

func refreshRepoFetchFailureNoticeTx(ctx context.Context, tx *writeTx, projectID int64, at int64, create bool) (bool, error) {
	failures, err := repoFetchFailuresTx(ctx, tx, projectID)
	if err != nil {
		return false, err
	}
	if len(failures) == 0 {
		result, err := tx.ExecContext(ctx, `UPDATE notices SET acked_at=COALESCE(acked_at,?) WHERE project_id=? AND kind='root_fetch_failing' AND acked_at IS NULL`, at, projectID)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		return changed > 0, err
	}

	data, err := json.Marshal(map[string]any{"consecutive_failures": repoFetchFailureNoticeAfter, "repositories": failures})
	if err != nil {
		return false, err
	}
	var names []string
	for _, failure := range failures {
		name := failure.Repo
		if name == "" {
			name = "Project"
		}
		names = append(names, name)
	}
	summary := "Origin fetch has failed for three consecutive attempts in " + strings.Join(names, ", ")
	var existingID int64
	var existingSummary, existingData string
	err = tx.QueryRowContext(ctx, `SELECT id,summary,data_json FROM notices WHERE project_id=? AND kind='root_fetch_failing' AND id>COALESCE((SELECT MAX(id) FROM notices WHERE project_id=? AND kind='root_fetch_recovered'),0) ORDER BY id DESC LIMIT 1`, projectID, projectID).Scan(&existingID, &existingSummary, &existingData)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if !create {
			return false, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, Kind: "root_fetch_failing", Summary: summary, DataJSON: string(data), CreatedAt: at}); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	if existingSummary == summary && existingData == string(data) {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE notices SET summary=?,data_json=? WHERE id=?`, summary, string(data), existingID)
	return err == nil, err
}

// RecordRootBehind raises one root_behind Notice per new upstream head of a
// repository: the Project itself (repo "") or one workspace member.
func (db *DB) RecordRootBehind(ctx context.Context, projectID int64, repo string, at int64, head string, behind int, reason string) (bool, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_watch_state(project_id) VALUES (?)`, projectID); err != nil {
		return false, err
	}
	var previous string
	if repo == "" {
		err = tx.QueryRowContext(ctx, `SELECT root_behind_head FROM project_watch_state WHERE project_id=?`, projectID).Scan(&previous)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT root_behind_head FROM repo_watch_state WHERE project_id=? AND repo=?`, projectID, repo).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
	}
	if err != nil {
		return false, err
	}
	dedupeHead := head
	if dedupeHead == "" {
		dedupeHead = "error:" + reason
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_watch_state SET checkout_checked_at=? WHERE project_id=?`, at, projectID); err != nil {
		return false, err
	}
	if err := setRootBehindHeadTx(ctx, tx, projectID, repo, dedupeHead); err != nil {
		return false, err
	}
	created := dedupeHead != previous
	if created {
		fields := map[string]any{"upstream_head": head, "commits_behind": behind, "reason": reason}
		subject := "Project checkout"
		if repo != "" {
			fields["repo"] = repo
			subject = repo + ": checkout"
		}
		data, err := json.Marshal(fields)
		if err != nil {
			return false, err
		}
		summary := subject + " is " + strconv.Itoa(behind) + " commits behind origin: " + reason
		if head == "" {
			summary = subject + " could not sync from origin: " + reason
		}
		if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, Kind: "root_behind", Summary: summary, DataJSON: string(data), CreatedAt: at}); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if created {
		if err := db.PersistProject(ctx, projectID); err != nil {
			return false, err
		}
	}
	return created, nil
}

func setRootBehindHeadTx(ctx context.Context, tx *writeTx, projectID int64, repo, head string) error {
	if repo == "" {
		_, err := tx.ExecContext(ctx, `UPDATE project_watch_state SET root_behind_head=? WHERE project_id=?`, head, projectID)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO repo_watch_state(project_id, repo, root_behind_head) VALUES (?, ?, ?) ON CONFLICT(project_id, repo) DO UPDATE SET root_behind_head=excluded.root_behind_head`, projectID, repo, head)
	return err
}

func (db *DB) ClearRootBehind(ctx context.Context, projectID int64, repo string, at int64) error {
	if repo != "" {
		return db.SetRepoRootBehindHead(ctx, projectID, repo, "")
	}
	_, err := db.ExecContext(ctx, `INSERT INTO project_watch_state(project_id, checkout_checked_at, root_behind_head) VALUES (?, ?, '') ON CONFLICT(project_id) DO UPDATE SET checkout_checked_at=excluded.checkout_checked_at, root_behind_head=''`, projectID, at)
	return err
}

func (db *DB) LatestPRObservation(ctx context.Context, taskID int64) (PRObservation, error) {
	var observation PRObservation
	err := db.QueryRowContext(ctx, `SELECT id, project_id, task_id, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at FROM pr_observations WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&observation.ID, &observation.ProjectID, &observation.TaskID, &observation.PRURL, &observation.HeadSHA, &observation.State, &observation.Checks, &observation.Review, &observation.Mergeable, &observation.MergeCommit, &observation.ObservedAt)
	return observation, err
}

func (db *DB) RecordPRObservation(ctx context.Context, observation PRObservation, effect PRObservationEffect, expected Task) (bool, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_watch_state(project_id) VALUES (?)`, observation.ProjectID); err != nil {
		return false, err
	}
	// This no-op write serializes observers before they compare the latest row.
	if _, err := tx.ExecContext(ctx, `UPDATE project_watch_state SET pr_polled_at=pr_polled_at WHERE project_id=?`, observation.ProjectID); err != nil {
		return false, err
	}
	previous, err := latestPRObservationTx(ctx, tx, observation.TaskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// Reject a poll whose Task changed while the forge request was in flight.
	// An old PR must never Merge a Task now following a different PR or head.
	var current Task
	if err := tx.QueryRowContext(ctx, `SELECT state, pr_url, gated_sha FROM tasks WHERE id=? AND project_id=?`, observation.TaskID, observation.ProjectID).Scan(&current.State, &current.PRURL, &current.GatedSHA); err != nil {
		return false, err
	}
	if current.PRURL != observation.PRURL || current.State != expected.State || current.GatedSHA != expected.GatedSHA || current.PRURL != expected.PRURL {
		return false, nil
	}
	recoveredWatch, err := resetPRTaskWatchFailuresTx(ctx, tx, observation.ProjectID, observation.TaskID, "", observation.PRURL, observation.ObservedAt)
	if err != nil {
		return false, err
	}
	unchanged := err == nil && samePRObservation(previous, observation)
	if !unchanged {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pr_observations(project_id, task_id, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at, confirmed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, observation.ProjectID, observation.TaskID, observation.PRURL, observation.HeadSHA, observation.State, observation.Checks, observation.Review, observation.Mergeable, observation.MergeCommit, observation.ObservedAt, observation.ObservedAt); err != nil {
			return false, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE pr_observations SET confirmed_at=? WHERE id=?`, observation.ObservedAt, previous.ID); err != nil {
		return false, err
	}
	allowChangedEffects := !unchanged || effect.TransitionTo == StateMerged && transitions[current.State][StateMerged]
	notices, err := filterMergeReadyNoticesTx(ctx, tx, observation.ProjectID, observation.TaskID, "", current.State, effect.Notices)
	if err != nil {
		return false, err
	}
	for _, notice := range notices {
		if !allowChangedEffects && notice.Kind != "merge_ready" {
			continue
		}
		if err := insertNoticeTx(ctx, tx, notice); err != nil {
			return false, err
		}
	}
	transitioned := false
	if effect.TransitionTo != "" {
		state := current.State
		if transitions[state][effect.TransitionTo] && (effect.TransitionTo == StateMerged || state == StateMerging) {
			if effect.MergedRef != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE tasks SET landed_ref=?, updated_at=? WHERE id=?`, effect.MergedRef, time.Now().UnixMilli(), observation.TaskID); err != nil {
					return false, err
				}
			}
			if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), observation.TaskID, state, effect.TransitionTo, "cli", effect.TransitionNote); err != nil {
				return false, err
			}
			transitioned = true
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	changed := !unchanged || transitioned || recoveredWatch
	if transitioned {
		if err := db.PersistTask(ctx, observation.TaskID); err != nil {
			return false, err
		}
	} else if changed {
		if err := db.PersistProject(ctx, observation.ProjectID); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func resetPRTaskWatchFailuresTx(ctx context.Context, tx *writeTx, projectID, taskID int64, repo, prURL string, at int64) (bool, error) {
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	var failures int
	err := tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM pr_task_watch_failures WHERE project_id=? AND task_id=? AND repo=? AND pr_url=?`, projectID, taskID, repo, prURL).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	recovered := failures >= 3
	if recovered {
		var taskSeq int
		if err := tx.QueryRowContext(ctx, `SELECT seq FROM tasks WHERE project_id=? AND id=?`, projectID, taskID).Scan(&taskSeq); err != nil {
			return false, err
		}
		fields := map[string]any{"pr_url": prURL}
		summary := "PR watch recovered for Task t" + strconv.Itoa(taskSeq)
		if repo != "" {
			fields["repo"] = repo
			summary += " (" + repo + ")"
		}
		data, err := json.Marshal(fields)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE notices SET acked_at=COALESCE(acked_at,?) WHERE project_id=? AND task_id=? AND kind='pr_watch_failing' AND json_extract(data_json,'$.pr_url')=? AND COALESCE(json_extract(data_json,'$.repo'),'')=?`, at, projectID, taskID, prURL, repo); err != nil {
			return false, err
		}
		if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, TaskID: taskID, Kind: "pr_watch_recovered", Summary: summary, DataJSON: string(data), CreatedAt: at}); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pr_task_watch_failures SET consecutive_failures=0,last_error='',updated_at=? WHERE project_id=? AND task_id=? AND repo=? AND pr_url=?`, at, projectID, taskID, repo, prURL); err != nil {
		return false, err
	}
	return recovered, nil
}

func latestPRObservationTx(ctx context.Context, tx *writeTx, taskID int64) (PRObservation, error) {
	var observation PRObservation
	err := tx.QueryRowContext(ctx, `SELECT id, project_id, task_id, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at FROM pr_observations WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&observation.ID, &observation.ProjectID, &observation.TaskID, &observation.PRURL, &observation.HeadSHA, &observation.State, &observation.Checks, &observation.Review, &observation.Mergeable, &observation.MergeCommit, &observation.ObservedAt)
	return observation, err
}

// PRObservationConfirmedAt is when the watch last read the observation's
// state from the forge, including polls that found it unchanged.
func (db *DB) PRObservationConfirmedAt(ctx context.Context, id int64) (int64, error) {
	var confirmed int64
	err := db.QueryRowContext(ctx, `SELECT confirmed_at FROM pr_observations WHERE id=?`, id).Scan(&confirmed)
	return confirmed, err
}

func samePRObservation(left, right PRObservation) bool {
	return left.ProjectID == right.ProjectID && left.TaskID == right.TaskID && left.PRURL == right.PRURL && left.HeadSHA == right.HeadSHA && left.State == right.State && left.Checks == right.Checks && left.Review == right.Review && left.Mergeable == right.Mergeable && left.MergeCommit == right.MergeCommit
}

func filterMergeReadyNoticesTx(ctx context.Context, tx *writeTx, projectID, taskID int64, repo string, taskState State, notices []Notice) ([]Notice, error) {
	if len(notices) == 0 {
		return nil, nil
	}
	var transitionID, transitionAt int64
	err := tx.QueryRowContext(ctx, `SELECT id,at FROM transitions WHERE task_id=? AND to_state='done' ORDER BY id DESC LIMIT 1`, taskID).Scan(&transitionID, &transitionAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	filtered := make([]Notice, 0, len(notices))
	for _, notice := range notices {
		if notice.Kind != "merge_ready" {
			filtered = append(filtered, notice)
			continue
		}
		if taskState != StateDone && taskState != StateMerging {
			continue
		}
		data := map[string]any{}
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
			return nil, fmt.Errorf("decode merge_ready Notice: %w", err)
		}
		data["completion_transition"] = transitionID
		encoded, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		notice.DataJSON = string(encoded)
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM notices
			WHERE project_id=? AND task_id=? AND kind='merge_ready'
			  AND COALESCE(json_extract(data_json,'$.repo'),'')=?
			  AND json_extract(data_json,'$.head_sha')=?
			  AND (json_extract(data_json,'$.completion_transition')=?
			       OR (json_type(data_json,'$.completion_transition') IS NULL AND ?>0 AND created_at>=?))
		)`, projectID, taskID, repo, data["head_sha"], transitionID, transitionID, transitionAt).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			filtered = append(filtered, notice)
		}
	}
	return filtered, nil
}

func insertNoticeTx(ctx context.Context, tx *writeTx, notice Notice) error {
	if notice.DataJSON == "" {
		notice.DataJSON = "{}"
	}
	createdAt := notice.CreatedAt
	if createdAt == 0 {
		createdAt = time.Now().UnixMilli()
	}
	var taskID sql.NullInt64
	if notice.TaskID != 0 {
		taskID = sql.NullInt64{Int64: notice.TaskID, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id, task_id, kind, summary, data_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`, notice.ProjectID, taskID, notice.Kind, notice.Summary, notice.DataJSON, createdAt)
	return err
}

func ProjectWatchInterval(last int64, interval time.Duration, now time.Time) bool {
	return last == 0 || now.UnixMilli()-last >= interval.Milliseconds()
}
