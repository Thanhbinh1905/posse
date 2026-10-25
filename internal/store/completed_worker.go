package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ObserveCompletedWorker reopens a completed Change Task only after an observed
// turn-ending status is followed by a new working status. A done Signal arms
// the current turn, so its closing reply cannot reopen the Task.
func (db *DB) ObserveCompletedWorker(ctx context.Context, expected Task, paneID, server, status string, now time.Time) (*Notice, error) {
	return db.ObserveCompletedWorkerState(ctx, expected, paneID, server, status, RepositoryFingerprints{}, now)
}

// RepositoryFingerprints reads the Task repository for a completed Worker
// observation. A nil reader counts as unreadable.
type RepositoryFingerprints struct {
	// TurnEnd pairs with a turn-ending status and must not postdate it, so a
	// following turn's change cannot become the baseline.
	TurnEnd func() (string, error)
	// Current reads the repository now.
	Current func() (string, error)
}

func readFingerprint(read func() (string, error)) (string, bool) {
	if read == nil {
		return "", false
	}
	value, err := read()
	return value, err == nil
}

// ObserveCompletedWorkerState also compares the Task repository fingerprint
// with the one recorded when the completed turn ended. A new turn that leaves
// it unchanged, such as a background-task wake that only acknowledges a result,
// keeps the done Signal valid. A changed or unreadable fingerprint reopens the
// Task and queues an instruction to signal again. A new turn's change can land
// after its last observed working status, so the turn's end compares the
// repository again.
func (db *DB) ObserveCompletedWorkerState(ctx context.Context, expected Task, paneID, server, status string, fingerprints RepositoryFingerprints, now time.Time) (*Notice, error) {
	if expected.Type != "change" || (expected.State != StateDone && expected.State != StateMerging) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ObservationWriteBudget)
	defer cancel()
	busy := status == "working"
	turnEnded := status == "idle" || status == "done"
	current, known := "", false
	if busy {
		current, known = readFingerprint(fingerprints.Current)
	} else if turnEnded {
		current, known = readFingerprint(fingerprints.TurnEnd)
	}
	// The end of a resumed turn compares the repository as it is now: a change
	// that lands after the turn-end baseline read still belongs to that turn.
	latest, latestKnown, latestRead := "", false, false

	// Avoid a write transaction on every idle observer tick after the turn
	// boundary has already been recorded.
	if turnEnded {
		var currentTransition, recordedTransition int64
		var recordedPane, recordedServer, recordedBaseline string
		var recordedTurnEnded, recordedResumed int
		err := db.QueryRowContext(ctx, `SELECT COALESCE((SELECT id FROM transitions WHERE task_id=tasks.id AND to_state='done' ORDER BY id DESC LIMIT 1),(SELECT MAX(id) FROM transitions WHERE task_id=tasks.id),0) FROM tasks WHERE id=?`, expected.ID).Scan(&currentTransition)
		if err != nil {
			return nil, err
		}
		err = db.QueryRowContext(ctx, `SELECT transition_id,pane_id,server_started_at,turn_ended,baseline,resumed FROM completed_rider_busy WHERE task_id=?`, expected.ID).Scan(&recordedTransition, &recordedPane, &recordedServer, &recordedTurnEnded, &recordedBaseline, &recordedResumed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		sameTurn := err == nil && recordedTransition == currentTransition && recordedPane == paneID && recordedServer == server
		if sameTurn && recordedTurnEnded != 0 && (!known || recordedBaseline == current) {
			return nil, nil
		}
		if sameTurn && recordedResumed != 0 {
			latest, latestKnown = readFingerprint(fingerprints.Current)
			latestRead = true
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
	if state != expected.State || taskType != "change" {
		return nil, ErrStateRace
	}
	if transitionID == 0 {
		return nil, tx.Commit()
	}
	var recordedTransition int64
	var recordedPane, recordedServer, recordedBaseline string
	var recordedTurnEnded, recordedResumed int
	err = tx.QueryRowContext(ctx, `SELECT transition_id,pane_id,server_started_at,turn_ended,baseline,resumed FROM completed_rider_busy WHERE task_id=?`, expected.ID).Scan(&recordedTransition, &recordedPane, &recordedServer, &recordedTurnEnded, &recordedBaseline, &recordedResumed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) || recordedTransition != transitionID || recordedPane != paneID || recordedServer != server {
		if err := upsertCompletedWorkerTurn(ctx, tx, expected.ID, transitionID, paneID, server, now.UnixMilli(), turnEnded, current); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	if known && recordedBaseline == "" && recordedTurnEnded == 0 && recordedResumed == 0 {
		// The done Signal armed this turn before any fingerprint was observed.
		if _, err := tx.ExecContext(ctx, `UPDATE completed_rider_busy SET baseline=? WHERE task_id=?`, current, expected.ID); err != nil {
			return nil, err
		}
		recordedBaseline = current
	}
	if turnEnded && recordedResumed != 0 {
		if !latestRead {
			// A concurrent observation resumed the turn after this one read the
			// repository. The next turn-end observation decides it.
			return nil, tx.Commit()
		}
		if !latestKnown || recordedBaseline == "" || recordedBaseline != latest {
			return db.reopenCompletedWorkerTx(ctx, tx, expected, state, paneID, status, now)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE completed_rider_busy SET turn_ended=1,resumed=0 WHERE task_id=?`, expected.ID); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	if turnEnded {
		// Idle changes are not a turn: Merge's branch_moved check covers a fast
		// turn that changed HEAD between observations.
		if _, err := tx.ExecContext(ctx, `UPDATE completed_rider_busy SET turn_ended=1,baseline=? WHERE task_id=?`, current, expected.ID); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	if !busy || (recordedTurnEnded == 0 && recordedResumed == 0) {
		return nil, tx.Commit()
	}
	// A turn that ended without a readable fingerprint has no baseline to
	// compare, so a new busy turn counts as a change.
	if !known || recordedBaseline == "" || recordedBaseline != current {
		return db.reopenCompletedWorkerTx(ctx, tx, expected, state, paneID, status, now)
	}
	if recordedTurnEnded != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE completed_rider_busy SET turn_ended=0,resumed=1 WHERE task_id=?`, expected.ID); err != nil {
			return nil, err
		}
	}
	return nil, tx.Commit()
}

func (db *DB) reopenCompletedWorkerTx(ctx context.Context, tx *writeTx, expected Task, state State, paneID, status string, now time.Time) (*Notice, error) {

	if err := cancelPendingMergeIntentTx(ctx, tx, expected.ID); err != nil {
		if errors.Is(err, ErrTaskMergeInProgress) || errors.Is(err, ErrIntentOwned) {
			return nil, tx.Commit()
		}
		return nil, err
	}
	note := fmt.Sprintf("Herdr observed a new working turn in pane %s after the completed Worker's turn ended", paneID)
	if err := validateTransitionSource(state, StateWorking, "watcher"); err != nil {
		return nil, err
	}
	if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), expected.ID, state, StateWorking, "watcher", note); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET gated_sha='' WHERE id=?`, expected.ID); err != nil {
		return nil, err
	}
	// Preserve already merged members, but clear every remaining merge head.
	if _, err := tx.ExecContext(ctx, `UPDATE task_repos SET gated_sha='',state=CASE WHEN state IN ('gated','unchanged','merging') THEN 'open' ELSE state END,updated_at=? WHERE task_id=? AND state<>'merged'`, now.UnixMilli(), expected.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM completed_rider_busy WHERE task_id=?`, expected.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE decisions SET obsolete_at=?,obsolete_reason='Task reopened after completed Worker turn' WHERE task_id=? AND kind='merge_ready' AND answered_at=0 AND obsolete_at=0`, now.UnixMilli(), expected.ID); err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]any{"pane_id": paneID, "agent_status": status, "turn_ended": true, "previous_state": state})
	if err != nil {
		return nil, err
	}
	notice := Notice{ProjectID: expected.ProjectID, TaskID: expected.ID, Kind: "worker_resumed", Summary: expected.Title + ": Worker resumed in a new turn after completion; Task returned to working and requires a new done Signal", DataJSON: string(data), CreatedAt: now.UnixMilli()}
	if err := insertNoticeTx(ctx, tx, notice); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&notice.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(task_id, body, created_at, status, wait_for_idle) VALUES(?,?,?,'queued',1)`, expected.ID, "Posse reopened this Task because a new turn changed its repository after your done Signal. Commit, publish if needed, and signal done again.", now.UnixMilli()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &notice, db.PersistTask(ctx, expected.ID)
}

func upsertCompletedWorkerTurn(ctx context.Context, tx *writeTx, taskID, transitionID int64, paneID, server string, observedAt int64, turnEnded bool, baseline string) error {
	ended := 0
	if turnEnded {
		ended = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO completed_rider_busy(task_id,transition_id,pane_id,server_started_at,busy_since,turn_ended,baseline,resumed) VALUES(?,?,?,?,?,?,?,0)
ON CONFLICT(task_id) DO UPDATE SET transition_id=excluded.transition_id,pane_id=excluded.pane_id,server_started_at=excluded.server_started_at,busy_since=excluded.busy_since,turn_ended=excluded.turn_ended,baseline=excluded.baseline,resumed=0`, taskID, transitionID, paneID, server, observedAt, ended, baseline)
	return err
}
