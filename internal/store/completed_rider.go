package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ObserveCompletedRider reopens a completed Ship Task after consecutive busy
// observations. The completion transition, pane and server identify an episode;
// a new done Signal or Rider identity restarts its grace period.
func (db *DB) ObserveCompletedRider(ctx context.Context, expected Task, paneID, server string, busy bool, now time.Time, grace time.Duration) (*Notice, error) {
	if expected.Type != "ship" || (expected.State != StateDone && expected.State != StateLanding) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ObservationWriteBudget)
	defer cancel()
	// Idle completed Tasks normally have no candidate. Avoid acquiring a write
	// lock on every watcher tick when there is nothing to reset.
	if !busy {
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM completed_rider_busy WHERE task_id=?`, expected.ID).Scan(&exists); err != nil || exists == 0 {
			return nil, err
		}
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state State
	var taskType string
	var transitionID int64
	if err := tx.QueryRowContext(ctx, `SELECT state,type,COALESCE((SELECT MAX(id) FROM transitions WHERE task_id=tasks.id),0) FROM tasks WHERE id=?`, expected.ID).Scan(&state, &taskType, &transitionID); err != nil {
		return nil, err
	}
	if state != expected.State || taskType != "ship" {
		return nil, ErrStateRace
	}
	if !busy {
		if _, err := tx.ExecContext(ctx, `DELETE FROM completed_rider_busy WHERE task_id=?`, expected.ID); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	var recordedTransition, since int64
	var recordedPane, recordedServer string
	err = tx.QueryRowContext(ctx, `SELECT transition_id,pane_id,server_started_at,busy_since FROM completed_rider_busy WHERE task_id=?`, expected.ID).Scan(&recordedTransition, &recordedPane, &recordedServer, &since)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) || recordedTransition != transitionID || recordedPane != paneID || recordedServer != server || now.UnixMilli() < since {
		_, err := tx.ExecContext(ctx, `INSERT INTO completed_rider_busy(task_id,transition_id,pane_id,server_started_at,busy_since) VALUES(?,?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET transition_id=excluded.transition_id,pane_id=excluded.pane_id,server_started_at=excluded.server_started_at,busy_since=excluded.busy_since`, expected.ID, transitionID, paneID, server, now.UnixMilli())
		if err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	if now.Sub(time.UnixMilli(since)) < grace {
		return nil, nil
	}
	note := fmt.Sprintf("Herdr reports Rider working in pane %s since %s across consecutive observations (at least %s); no Posse instruction reopened the Task", paneID, time.UnixMilli(since).UTC().Format(time.RFC3339Nano), grace)
	if err := validateTransitionSource(state, StateWorking, "watcher"); err != nil {
		return nil, err
	}
	if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), expected.ID, state, StateWorking, "watcher", note); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET gated_sha='' WHERE id=?`, expected.ID); err != nil {
		return nil, err
	}
	// Preserve already merged members, but invalidate every remaining gate.
	if _, err := tx.ExecContext(ctx, `UPDATE task_repos SET gated_sha='',state=CASE WHEN state IN ('gated','unchanged') THEN 'open' ELSE state END,updated_at=? WHERE task_id=? AND state<>'landed'`, now.UnixMilli(), expected.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM completed_rider_busy WHERE task_id=?`, expected.ID); err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]any{"pane_id": paneID, "agent_status": "working", "busy_since": since, "previous_state": state})
	if err != nil {
		return nil, err
	}
	notice := Notice{ProjectID: expected.ProjectID, TaskID: expected.ID, Kind: "rider_resumed", Summary: expected.Title + ": Rider resumed work outside Posse instruction delivery; Task returned to working and requires a new done Signal", DataJSON: string(data), CreatedAt: now.UnixMilli()}
	if err := insertNoticeTx(ctx, tx, notice); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&notice.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &notice, db.PersistTask(ctx, expected.ID)
}
