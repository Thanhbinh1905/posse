package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const modelErrorEpisodeColumns = `SELECT task_id,episode,launch,agent,kind,fingerprint,attempts,turn_state,status,started_at,next_attempt_at,updated_at FROM model_error_episodes WHERE task_id=?`

var ErrModelErrorPromptNotAllowed = errors.New("model-error prompt is no longer eligible")

const (
	modelErrorTurnReady = iota
	modelErrorTurnAwaitingWork
	modelErrorTurnStartedDuringSubmission
)

type ModelErrorEpisode struct {
	TaskID        int64  `json:"task_id"`
	Episode       int    `json:"episode"`
	Launch        int    `json:"launch"`
	Agent         string `json:"agent"`
	Kind          string `json:"kind"`
	Fingerprint   string `json:"fingerprint"`
	Attempts      int    `json:"attempts"`
	TurnState     int    `json:"turn_state"`
	Status        string `json:"status"`
	StartedAt     int64  `json:"started_at"`
	NextAttemptAt int64  `json:"next_attempt_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

func (db *DB) TaskModelErrorEpisode(ctx context.Context, taskID int64) (ModelErrorEpisode, error) {
	var episode ModelErrorEpisode
	err := db.QueryRowContext(ctx, `SELECT task_id,episode,launch,agent,kind,fingerprint,attempts,turn_state,status,started_at,next_attempt_at,updated_at
FROM model_error_episodes WHERE task_id=?`, taskID).Scan(
		&episode.TaskID, &episode.Episode, &episode.Launch, &episode.Agent, &episode.Kind,
		&episode.Fingerprint, &episode.Attempts, &episode.TurnState, &episode.Status, &episode.StartedAt,
		&episode.NextAttemptAt, &episode.UpdatedAt,
	)
	if IsNotFound(err) {
		return ModelErrorEpisode{}, nil
	}
	return episode, err
}

// ObserveModelErrorEpisode starts an episode or records an end-of-turn
// observation. The persisted turn state admits a new observation only after
// Herdr reports working following the prior continue; duplicate idle hooks
// remain deduplicated even when the terminal screen is repainted identically.
func (db *DB) ObserveModelErrorEpisode(ctx context.Context, taskID int64, launch int, agent, kind, fingerprint string, now int64) (ModelErrorEpisode, bool, error) {
	if taskID < 1 || launch < 1 || agent == "" || kind == "" || fingerprint == "" {
		return ModelErrorEpisode{}, false, fmt.Errorf("model error episode identity is incomplete")
	}
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return ModelErrorEpisode{}, false, err
	}
	defer tx.Rollback()
	var episode ModelErrorEpisode
	err = tx.QueryRowContext(ctx, `SELECT task_id,episode,launch,agent,kind,fingerprint,attempts,turn_state,status,started_at,next_attempt_at,updated_at
FROM model_error_episodes WHERE task_id=?`, taskID).Scan(
		&episode.TaskID, &episode.Episode, &episode.Launch, &episode.Agent, &episode.Kind,
		&episode.Fingerprint, &episode.Attempts, &episode.TurnState, &episode.Status, &episode.StartedAt,
		&episode.NextAttemptAt, &episode.UpdatedAt,
	)
	if err != nil && !IsNotFound(err) {
		return ModelErrorEpisode{}, false, err
	}
	if err == nil && episode.Launch == launch && episode.Kind == kind && episode.Status == "active" && (episode.NextAttemptAt > 0 || episode.TurnState != modelErrorTurnReady) {
		return episode, false, tx.Commit()
	}
	if err == nil && episode.Launch == launch && episode.Kind == kind && episode.Fingerprint == fingerprint && episode.Status != "resolved" && (episode.Status != "active" || episode.Attempts == 0) {
		return episode, false, tx.Commit()
	}
	if err == nil && episode.Launch == launch && episode.Kind == kind && episode.Status == "active" {
		if _, err := tx.ExecContext(ctx, `UPDATE model_error_episodes SET fingerprint=?,updated_at=? WHERE task_id=? AND episode=?`, fingerprint, now, taskID, episode.Episode); err != nil {
			return ModelErrorEpisode{}, false, err
		}
		episode.Fingerprint = fingerprint
		episode.UpdatedAt = now
		if err := insertModelErrorSignal(ctx, tx, episode, "model_error_detected", "Recognized a model error at the end of a Rider turn", nil, now); err != nil {
			return ModelErrorEpisode{}, false, err
		}
		return episode, true, tx.Commit()
	}
	if err == nil {
		episode.Episode++
	} else {
		episode.Episode = 1
	}
	episode.TaskID = taskID
	episode.Launch = launch
	episode.Agent = agent
	episode.Kind = kind
	episode.Fingerprint = fingerprint
	episode.Attempts = 0
	episode.Status = "active"
	episode.StartedAt = now
	episode.NextAttemptAt = 0
	episode.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_error_episodes(task_id,episode,launch,agent,kind,fingerprint,attempts,turn_state,status,started_at,next_attempt_at,updated_at)
VALUES(?,?,?,?,?,?,0,0,'active',?,0,?)
ON CONFLICT(task_id) DO UPDATE SET episode=excluded.episode,launch=excluded.launch,agent=excluded.agent,kind=excluded.kind,fingerprint=excluded.fingerprint,attempts=0,turn_state=0,status='active',started_at=excluded.started_at,next_attempt_at=0,updated_at=excluded.updated_at`,
		taskID, episode.Episode, launch, agent, kind, fingerprint, now, now); err != nil {
		return ModelErrorEpisode{}, false, err
	}
	if err := insertModelErrorSignal(ctx, tx, episode, "model_error_detected", "Recognized a model error at the end of a Rider turn", nil, now); err != nil {
		return ModelErrorEpisode{}, false, err
	}
	return episode, true, tx.Commit()
}

// WithModelErrorPrompt serializes automatic submission against Task Signals.
// The write transaction is held through the Herdr call so a needs-decision or
// terminal transition either commits first and blocks this prompt, or follows
// a prompt that was already submitted.
func (db *DB) WithModelErrorPrompt(ctx context.Context, task Task, episodeNumber int, submit func() error) error {
	if submit == nil {
		return fmt.Errorf("model-error prompt submission is required")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	var launch int
	var paneID, paneLabel, workspaceID string
	if err := tx.QueryRowContext(ctx, `SELECT state,launches,pane_id,pane_label,herdr_workspace_id FROM tasks WHERE id=?`, task.ID).Scan(&state, &launch, &paneID, &paneLabel, &workspaceID); err != nil {
		return err
	}
	if State(state) != StateWorking || launch != task.Launches || paneID != task.PaneID || paneLabel != task.PaneLabel || workspaceID != task.HerdrWorkspaceID {
		return ErrModelErrorPromptNotAllowed
	}
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, modelErrorEpisodeColumns, task.ID))
	if err != nil {
		return err
	}
	if episode.Episode != episodeNumber || episode.Launch != launch || episode.Status != "active" {
		return ErrModelErrorPromptNotAllowed
	}
	var queued int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE task_id=? AND status='queued')`, task.ID).Scan(&queued); err != nil {
		return err
	}
	if queued != 0 {
		return ErrModelErrorPromptNotAllowed
	}
	if err := submit(); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimModelErrorNudge charges the bounded retry budget before the prompt can
// be submitted, so a crash cannot turn one episode into unlimited retries.
func (db *DB) ClaimModelErrorNudge(ctx context.Context, taskID int64, episodeNumber int, fingerprint string, limit int, now, backoffMillis int64) (ModelErrorEpisode, bool, error) {
	if limit < 1 || backoffMillis < 0 {
		return ModelErrorEpisode{}, false, fmt.Errorf("model error retry limit and backoff are invalid")
	}
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return ModelErrorEpisode{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE model_error_episodes SET attempts=attempts+1,next_attempt_at=?,updated_at=?
WHERE task_id=? AND episode=? AND fingerprint=? AND status='active' AND attempts<? AND next_attempt_at<=?`, now+backoffMillis, now, taskID, episodeNumber, fingerprint, limit, now)
	if err != nil {
		return ModelErrorEpisode{}, false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return ModelErrorEpisode{}, false, err
	}
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, modelErrorEpisodeColumns, taskID))
	if err != nil {
		return ModelErrorEpisode{}, false, err
	}
	return episode, updated == 1, tx.Commit()
}

func (db *DB) CompleteModelErrorNudge(ctx context.Context, taskID int64, episodeNumber int, backoffMillis int64, marker string, now int64) error {
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE model_error_episodes SET next_attempt_at=0,turn_state=CASE WHEN turn_state=? THEN ? ELSE ? END,updated_at=? WHERE task_id=? AND episode=? AND status='active'`, modelErrorTurnStartedDuringSubmission, modelErrorTurnReady, modelErrorTurnAwaitingWork, now, taskID, episodeNumber)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil || updated == 0 {
		return errors.Join(err, tx.Commit())
	}
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, modelErrorEpisodeColumns, taskID))
	if err != nil {
		return err
	}
	data := map[string]any{"attempt": episode.Attempts, "max_attempts": modelErrorNudgeLimitForSignal, "backoff_ms": backoffMillis, "marker": marker}
	if err := insertModelErrorSignal(ctx, tx, episode, "model_stream_error_retry", fmt.Sprintf("Sent continue nudge %d/%d after a transient model stream disconnect", episode.Attempts, modelErrorNudgeLimitForSignal), data, now); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkModelErrorTurnStarted opens the next failed-turn slot only after the
// harness has reported that the Rider resumed work following a nudge.
func (db *DB) MarkModelErrorTurnStarted(ctx context.Context, taskID, launch int64, now int64) error {
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	_, err := db.ExecContext(ctx, `UPDATE model_error_episodes SET turn_state=CASE WHEN next_attempt_at>0 THEN ? ELSE ? END,updated_at=? WHERE task_id=? AND launch=? AND status='active' AND (turn_state=? OR next_attempt_at>0)`, modelErrorTurnStartedDuringSubmission, modelErrorTurnReady, now, taskID, launch, modelErrorTurnAwaitingWork)
	return err
}

const (
	modelErrorNudgeLimitForSignal = 3
	modelErrorNudgeClaimLease     = 5 * time.Second
)

// ModelErrorNudgeExpired reports a charged claim that remained incomplete
// beyond its backoff deadline and a grace lease for the submission process.
func ModelErrorNudgeExpired(episode ModelErrorEpisode, now time.Time) bool {
	return episode.NextAttemptAt > 0 && episode.NextAttemptAt+modelErrorNudgeClaimLease.Milliseconds() <= now.UnixMilli()
}

func ModelErrorAwaitingTurn(episode ModelErrorEpisode) bool {
	return episode.Status == "active" && episode.TurnState == modelErrorTurnAwaitingWork && episode.NextAttemptAt == 0
}

// InterruptExpiredModelErrorNudge records an explicit Notice when a charged
// retry claim outlives its submission window. A crash may have happened either
// before or after Herdr accepted the prompt, so the claim is never replayed.
func (db *DB) InterruptExpiredModelErrorNudge(ctx context.Context, taskID int64, now int64, notice Notice) (*Notice, error) {
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, modelErrorEpisodeColumns, taskID))
	if IsNotFound(err) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if episode.Status != "active" || !ModelErrorNudgeExpired(episode, time.UnixMilli(now)) {
		return nil, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_error_episodes SET status='interrupted',next_attempt_at=0,updated_at=? WHERE task_id=? AND episode=? AND status='active' AND next_attempt_at=? AND next_attempt_at+?<=?`, now, taskID, episode.Episode, episode.NextAttemptAt, modelErrorNudgeClaimLease.Milliseconds(), now)
	if err != nil {
		return nil, err
	}
	updated, err := result.RowsAffected()
	if err != nil || updated == 0 {
		return nil, errors.Join(err, tx.Commit())
	}
	if notice.ProjectID < 1 || notice.TaskID != taskID || notice.Kind == "" || notice.Summary == "" {
		return nil, fmt.Errorf("model error interruption Notice identity is incomplete")
	}
	if notice.DataJSON == "" {
		notice.DataJSON = "{}"
	}
	notice.CreatedAt = now
	inserted, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,?,?,?,?)`, notice.ProjectID, notice.TaskID, notice.Kind, notice.Summary, notice.DataJSON, now)
	if err != nil {
		return nil, err
	}
	notice.ID, err = inserted.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := insertModelErrorSignal(ctx, tx, episode, "model_stream_error_interrupted", "A charged continue attempt did not record completion; Posse did not replay it because delivery is uncertain", nil, now); err != nil {
		return nil, err
	}
	return &notice, tx.Commit()
}

// FinishModelErrorEpisode changes the current episode once and optionally
// commits its specific Notice in the same transaction.
func (db *DB) FinishModelErrorEpisode(ctx context.Context, taskID int64, episodeNumber int, status, note string, notice *Notice, now int64) (*Notice, error) {
	switch status {
	case "resolved", "refused", "exhausted", "blocked", "interrupted":
	default:
		return nil, fmt.Errorf("invalid model error episode status %q", status)
	}
	if now == 0 {
		now = time.Now().UnixMilli()
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, modelErrorEpisodeColumns, taskID))
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_error_episodes SET status=?,next_attempt_at=0,updated_at=?
WHERE task_id=? AND episode=? AND status='active'`, status, now, taskID, episodeNumber)
	if err != nil {
		return nil, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if updated == 0 {
		return nil, tx.Commit()
	}
	verb := map[string]string{"resolved": "model_error_resolved", "refused": "model_refused", "exhausted": "model_stream_error_exhausted", "blocked": "model_stream_error_withheld", "interrupted": "model_stream_error_interrupted"}[status]
	if err := insertModelErrorSignal(ctx, tx, episode, verb, note, nil, now); err != nil {
		return nil, err
	}
	if notice == nil {
		return nil, tx.Commit()
	}
	if notice.ProjectID < 1 || notice.TaskID != taskID || notice.Kind == "" || notice.Summary == "" {
		return nil, fmt.Errorf("model error Notice identity is incomplete")
	}
	if notice.DataJSON == "" {
		notice.DataJSON = "{}"
	}
	notice.CreatedAt = now
	inserted, err := tx.ExecContext(ctx, `INSERT INTO notices(project_id,task_id,kind,summary,data_json,created_at) VALUES(?,?,?,?,?,?)`, notice.ProjectID, notice.TaskID, notice.Kind, notice.Summary, notice.DataJSON, now)
	if err != nil {
		return nil, err
	}
	noticeID, err := inserted.LastInsertId()
	if err != nil {
		return nil, err
	}
	notice.ID = noticeID
	return notice, tx.Commit()
}

func blockModelErrorEpisodeTx(ctx context.Context, tx *writeTx, taskID int64, note string, now int64) error {
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, modelErrorEpisodeColumns, taskID))
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if episode.Status != "active" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_error_episodes SET status='blocked',next_attempt_at=0,updated_at=? WHERE task_id=? AND episode=? AND status='active'`, now, taskID, episode.Episode)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil || updated == 0 {
		return err
	}
	return insertModelErrorSignal(ctx, tx, episode, "model_stream_error_withheld", note, nil, now)
}

func insertModelErrorSignal(ctx context.Context, tx *writeTx, episode ModelErrorEpisode, verb, note string, extra map[string]any, now int64) error {
	data := map[string]any{"episode": episode.Episode, "launch": episode.Launch, "agent": episode.Agent, "kind": episode.Kind, "fingerprint": episode.Fingerprint, "attempts": episode.Attempts, "max_attempts": modelErrorNudgeLimitForSignal}
	for key, value := range extra {
		data[key] = value
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO signals(task_id,verb,note,data_json,at) VALUES(?,?,?,?,?)`, episode.TaskID, verb, note, string(encoded), now)
	return err
}

func scanModelErrorEpisode(row *sql.Row) (ModelErrorEpisode, error) {
	var episode ModelErrorEpisode
	err := row.Scan(&episode.TaskID, &episode.Episode, &episode.Launch, &episode.Agent, &episode.Kind,
		&episode.Fingerprint, &episode.Attempts, &episode.TurnState, &episode.Status, &episode.StartedAt,
		&episode.NextAttemptAt, &episode.UpdatedAt)
	return episode, err
}
