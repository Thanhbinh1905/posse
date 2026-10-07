package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type ModelErrorEpisode struct {
	TaskID        int64  `json:"task_id"`
	Episode       int    `json:"episode"`
	Launch        int    `json:"launch"`
	Agent         string `json:"agent"`
	Kind          string `json:"kind"`
	Fingerprint   string `json:"fingerprint"`
	Attempts      int    `json:"attempts"`
	Status        string `json:"status"`
	StartedAt     int64  `json:"started_at"`
	NextAttemptAt int64  `json:"next_attempt_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

func (db *DB) TaskModelErrorEpisode(ctx context.Context, taskID int64) (ModelErrorEpisode, error) {
	var episode ModelErrorEpisode
	err := db.QueryRowContext(ctx, `SELECT task_id,episode,launch,agent,kind,fingerprint,attempts,status,started_at,next_attempt_at,updated_at
FROM model_error_episodes WHERE task_id=?`, taskID).Scan(
		&episode.TaskID, &episode.Episode, &episode.Launch, &episode.Agent, &episode.Kind,
		&episode.Fingerprint, &episode.Attempts, &episode.Status, &episode.StartedAt,
		&episode.NextAttemptAt, &episode.UpdatedAt,
	)
	if IsNotFound(err) {
		return ModelErrorEpisode{}, nil
	}
	return episode, err
}

// ObserveModelErrorEpisode starts an episode or records a new end-of-turn
// observation. Identical events are deduplicated across concurrent hooks.
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
	err = tx.QueryRowContext(ctx, `SELECT task_id,episode,launch,agent,kind,fingerprint,attempts,status,started_at,next_attempt_at,updated_at
FROM model_error_episodes WHERE task_id=?`, taskID).Scan(
		&episode.TaskID, &episode.Episode, &episode.Launch, &episode.Agent, &episode.Kind,
		&episode.Fingerprint, &episode.Attempts, &episode.Status, &episode.StartedAt,
		&episode.NextAttemptAt, &episode.UpdatedAt,
	)
	if err != nil && !IsNotFound(err) {
		return ModelErrorEpisode{}, false, err
	}
	if err == nil && episode.Launch == launch && episode.Kind == kind && episode.Fingerprint == fingerprint && episode.Status != "resolved" {
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO model_error_episodes(task_id,episode,launch,agent,kind,fingerprint,attempts,status,started_at,next_attempt_at,updated_at)
VALUES(?,?,?,?,?,?,0,'active',?,0,?)
ON CONFLICT(task_id) DO UPDATE SET episode=excluded.episode,launch=excluded.launch,agent=excluded.agent,kind=excluded.kind,fingerprint=excluded.fingerprint,attempts=0,status='active',started_at=excluded.started_at,next_attempt_at=0,updated_at=excluded.updated_at`,
		taskID, episode.Episode, launch, agent, kind, fingerprint, now, now); err != nil {
		return ModelErrorEpisode{}, false, err
	}
	if err := insertModelErrorSignal(ctx, tx, episode, "model_error_detected", "Recognized a model error at the end of a Rider turn", nil, now); err != nil {
		return ModelErrorEpisode{}, false, err
	}
	return episode, true, tx.Commit()
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
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, `SELECT task_id,episode,launch,agent,kind,fingerprint,attempts,status,started_at,next_attempt_at,updated_at
FROM model_error_episodes WHERE task_id=?`, taskID))
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
	result, err := tx.ExecContext(ctx, `UPDATE model_error_episodes SET next_attempt_at=0,updated_at=? WHERE task_id=? AND episode=? AND status='active'`, now, taskID, episodeNumber)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil || updated == 0 {
		return errors.Join(err, tx.Commit())
	}
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, `SELECT task_id,episode,launch,agent,kind,fingerprint,attempts,status,started_at,next_attempt_at,updated_at FROM model_error_episodes WHERE task_id=?`, taskID))
	if err != nil {
		return err
	}
	data := map[string]any{"attempt": episode.Attempts, "max_attempts": modelErrorNudgeLimitForSignal, "backoff_ms": backoffMillis, "marker": marker}
	if err := insertModelErrorSignal(ctx, tx, episode, "model_stream_error_retry", fmt.Sprintf("Sent continue nudge %d/%d after a transient model stream disconnect", episode.Attempts, modelErrorNudgeLimitForSignal), data, now); err != nil {
		return err
	}
	return tx.Commit()
}

const modelErrorNudgeLimitForSignal = 3

// FinishModelErrorEpisode changes the current episode once and optionally
// commits its specific Notice in the same transaction.
func (db *DB) FinishModelErrorEpisode(ctx context.Context, taskID int64, episodeNumber int, status, note string, notice *Notice, now int64) (*Notice, error) {
	switch status {
	case "resolved", "refused", "exhausted", "blocked":
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
	episode, err := scanModelErrorEpisode(tx.QueryRowContext(ctx, `SELECT task_id,episode,launch,agent,kind,fingerprint,attempts,status,started_at,next_attempt_at,updated_at FROM model_error_episodes WHERE task_id=?`, taskID))
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
	verb := map[string]string{"resolved": "model_error_resolved", "refused": "model_refused", "exhausted": "model_stream_error_exhausted", "blocked": "model_stream_error_withheld"}[status]
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
		&episode.Fingerprint, &episode.Attempts, &episode.Status, &episode.StartedAt,
		&episode.NextAttemptAt, &episode.UpdatedAt)
	return episode, err
}
