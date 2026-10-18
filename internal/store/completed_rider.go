package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ObserveCompletedRider reopens a completed Ship Task only after an observed
// turn-ending status is followed by a new working status. A done Signal arms
// the current turn, so its closing reply cannot reopen the Task.
func (db *DB) ObserveCompletedRider(ctx context.Context, expected Task, paneID, server, status string, now time.Time) (*Notice, error) {
	if expected.Type != "ship" || (expected.State != StateDone && expected.State != StateLanding) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ObservationWriteBudget)
	defer cancel()
	busy := status == "working"
	turnEnded := status == "idle" || status == "done"

	// Avoid a write transaction on every idle observer tick after the turn
	// boundary has already been recorded.
	if turnEnded {
		var currentTransition, recordedTransition int64
		var recordedPane, recordedServer string
		var recordedTurnEnded int
		err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT id FROM transitions WHERE task_id=tasks.id AND to_state='done' ORDER BY id DESC LIMIT 1),(SELECT MAX(id) FROM transitions WHERE task_id=tasks.id),0) FROM tasks WHERE id=?`, expected.ID).Scan(&currentTransition)
		if err != nil {
			return nil, err
		}
		err = db.QueryRowContext(ctx, `SELECT transition_id,pane_id,server_started_at,turn_ended FROM completed_rider_busy WHERE task_id=?`, expected.ID).Scan(&recordedTransition, &recordedPane, &recordedServer, &recordedTurnEnded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && recordedTransition == currentTransition && recordedPane == paneID && recordedServer == server && recordedTurnEnded != 0 {
			return nil, nil
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
	if err := tx.QueryRowContext(ctx, `SELECT state,type,COALESCE((SELECT id FROM transitions WHERE task_id=tasks.id AND to_state='done' ORDER BY id DESC LIMIT 1),(SELECT MAX(id) FROM transitions WHERE task_id=tasks.id),0) FROM tasks WHERE id=?`, expected.ID).Scan(&state, &taskType, &transitionID); err != nil {
		return nil, err
	}
	if state != expected.State || taskType != "ship" {
		return nil, ErrStateRace
	}
	if transitionID == 0 {
		return nil, tx.Commit()
	}
	var recordedTransition int64
	var recordedPane, recordedServer string
	var recordedTurnEnded int
	err = tx.QueryRowContext(ctx, `SELECT transition_id,pane_id,server_started_at,turn_ended FROM completed_rider_busy WHERE task_id=?`, expected.ID).Scan(&recordedTransition, &recordedPane, &recordedServer, &recordedTurnEnded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) || recordedTransition != transitionID || recordedPane != paneID || recordedServer != server {
		if err := upsertCompletedRiderTurn(ctx, tx, expected.ID, transitionID, paneID, server, now.UnixMilli(), turnEnded); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	if turnEnded {
		if recordedTurnEnded == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE completed_rider_busy SET turn_ended=1 WHERE task_id=?`, expected.ID); err != nil {
				return nil, err
			}
		}
		return nil, tx.Commit()
	}
	if !busy || recordedTurnEnded == 0 {
		return nil, tx.Commit()
	}

	if err := cancelPendingLandIntentTx(ctx, tx, expected.ID); err != nil {
		if errors.Is(err, ErrTaskMergeInProgress) || errors.Is(err, ErrIntentOwned) {
			return nil, tx.Commit()
		}
		return nil, err
	}
	note := fmt.Sprintf("Herdr observed a new working turn in pane %s after the completed Rider's turn ended", paneID)
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
	if _, err := tx.ExecContext(ctx, `UPDATE task_repos SET gated_sha='',state=CASE WHEN state IN ('gated','unchanged','landing') THEN 'open' ELSE state END,updated_at=? WHERE task_id=? AND state<>'landed'`, now.UnixMilli(), expected.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM completed_rider_busy WHERE task_id=?`, expected.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason='Task reopened after completed Rider turn' WHERE task_id=? AND kind='land_ready' AND answered_at=0 AND obsolete_at=0`, now.UnixMilli(), expected.ID); err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]any{"pane_id": paneID, "agent_status": status, "turn_ended": true, "previous_state": state})
	if err != nil {
		return nil, err
	}
	notice := Notice{ProjectID: expected.ProjectID, TaskID: expected.ID, Kind: "rider_resumed", Summary: expected.Title + ": Rider resumed in a new turn after completion; Task returned to working and requires a new done Signal", DataJSON: string(data), CreatedAt: now.UnixMilli()}
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

func upsertCompletedRiderTurn(ctx context.Context, tx *writeTx, taskID, transitionID int64, paneID, server string, observedAt int64, turnEnded bool) error {
	ended := 0
	if turnEnded {
		ended = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO completed_rider_busy(task_id,transition_id,pane_id,server_started_at,busy_since,turn_ended) VALUES(?,?,?,?,?,?)
ON CONFLICT(task_id) DO UPDATE SET transition_id=excluded.transition_id,pane_id=excluded.pane_id,server_started_at=excluded.server_started_at,busy_since=excluded.busy_since,turn_ended=excluded.turn_ended`, taskID, transitionID, paneID, server, observedAt, ended)
	return err
}
