package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
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

type ProjectWatchState struct {
	PRPolledAt            int64
	PRConsecutiveFailures int
	CheckoutCheckedAt     int64
	RootBehindHead        string
}

type PRObservationEffect struct {
	Notices        []Notice
	TransitionTo   State
	TransitionNote string
	LandedRef      string
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
// the conditional update across the Lead and Lookout processes. The token
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
	result, err := db.ExecContext(ctx, `UPDATE project_watch_state SET pr_poll_claim_until=?,pr_poll_claim_token=? WHERE project_id=? AND pr_poll_claim_until<=? AND pr_polled_at<=?`, stamp+int64((5*time.Minute)/time.Millisecond), token, projectID, stamp, threshold)
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
	result, err := db.ExecContext(ctx, `UPDATE member_pr_poll_state SET claim_until=?,claim_token=? WHERE project_id=? AND repo=? AND claim_until<=? AND pr_polled_at<=?`, stamp+int64((5*time.Minute)/time.Millisecond), token, projectID, repo, stamp, threshold)
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

func (db *DB) RecordPRPoll(ctx context.Context, projectID int64, at int64, failureSummary string) (int, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_watch_state(project_id) VALUES (?)`, projectID); err != nil {
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
	if failures == 3 {
		data, err := json.Marshal(map[string]any{"consecutive_failures": failures, "error": failureSummary})
		if err != nil {
			return 0, err
		}
		if err := insertNoticeTx(ctx, tx, Notice{ProjectID: projectID, Kind: "pr_watch_failing", Summary: "Pull request polling failed three times: " + failureSummary, DataJSON: string(data), CreatedAt: at}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if failures == 3 {
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
	// An old PR must never Land a Task now following a different PR or head.
	var current Task
	if err := tx.QueryRowContext(ctx, `SELECT state, pr_url, gated_sha FROM tasks WHERE id=? AND project_id=?`, observation.TaskID, observation.ProjectID).Scan(&current.State, &current.PRURL, &current.GatedSHA); err != nil {
		return false, err
	}
	if current.PRURL != observation.PRURL || current.State != expected.State || current.GatedSHA != expected.GatedSHA || current.PRURL != expected.PRURL {
		return false, nil
	}
	unchanged := err == nil && samePRObservation(previous, observation)
	if !unchanged {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pr_observations(project_id, task_id, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, observation.ProjectID, observation.TaskID, observation.PRURL, observation.HeadSHA, observation.State, observation.Checks, observation.Review, observation.Mergeable, observation.MergeCommit, observation.ObservedAt); err != nil {
			return false, err
		}
	}
	if !unchanged || effect.TransitionTo == StateLanded && transitions[current.State][StateLanded] {
		for _, notice := range effect.Notices {
			if err := insertNoticeTx(ctx, tx, notice); err != nil {
				return false, err
			}
		}
	}
	transitioned := false
	if effect.TransitionTo != "" {
		state := current.State
		if transitions[state][effect.TransitionTo] && (effect.TransitionTo == StateLanded || state == StateLanding) {
			if effect.LandedRef != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE tasks SET landed_ref=?, updated_at=? WHERE id=?`, effect.LandedRef, time.Now().UnixMilli(), observation.TaskID); err != nil {
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
	changed := !unchanged || transitioned
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

func latestPRObservationTx(ctx context.Context, tx *writeTx, taskID int64) (PRObservation, error) {
	var observation PRObservation
	err := tx.QueryRowContext(ctx, `SELECT id, project_id, task_id, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at FROM pr_observations WHERE task_id=? ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&observation.ID, &observation.ProjectID, &observation.TaskID, &observation.PRURL, &observation.HeadSHA, &observation.State, &observation.Checks, &observation.Review, &observation.Mergeable, &observation.MergeCommit, &observation.ObservedAt)
	return observation, err
}

func samePRObservation(left, right PRObservation) bool {
	return left.ProjectID == right.ProjectID && left.TaskID == right.TaskID && left.PRURL == right.PRURL && left.HeadSHA == right.HeadSHA && left.State == right.State && left.Checks == right.Checks && left.Review == right.Review && left.Mergeable == right.Mergeable && left.MergeCommit == right.MergeCommit
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
