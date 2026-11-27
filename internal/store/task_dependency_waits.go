package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrInvalidTaskDependency = errors.New("invalid Task dependency")

func taskDependencyLanded(state State, landedRef string) bool {
	return state == StateMerged || state == StateTornDown && landedRef != ""
}

func recordTaskDependencyWait(ctx context.Context, tx *writeTx, task Task, ref string) error {
	seq := mustTaskSequence(ref)
	if seq < 1 || ref != fmt.Sprintf("t%d", seq) {
		return fmt.Errorf("%w: --on must name a Task ID such as t12", ErrInvalidTaskDependency)
	}
	if seq == task.Seq {
		return fmt.Errorf("%w: a Task cannot depend on itself", ErrInvalidTaskDependency)
	}
	var id int64
	var kind, landedRef string
	var state State
	err := tx.QueryRowContext(ctx, `SELECT id,type,state,landed_ref FROM tasks WHERE project_id=? AND seq=?`, task.ProjectID, seq).Scan(&id, &kind, &state, &landedRef)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s does not exist in this Project", ErrInvalidTaskDependency, ref)
	}
	if err != nil {
		return err
	}
	if kind != "change" {
		return fmt.Errorf("%w: a landed prerequisite must be a Change Task", ErrInvalidTaskDependency)
	}
	if taskDependencyLanded(state, landedRef) {
		return fmt.Errorf("%w: %s already landed", ErrInvalidTaskDependency, ref)
	}
	if state == StateDone {
		var noChange bool
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT json_extract(data_json,'$.no_change') FROM signals
			WHERE task_id=? AND verb='done' ORDER BY id DESC LIMIT 1),0)`, id).Scan(&noChange); err != nil {
			return err
		}
		if noChange {
			return fmt.Errorf("%w: %s completed without changes and cannot land", ErrInvalidTaskDependency, ref)
		}
	}
	if !oneOfState(state, StateSpawning, StateWorking, StateStalled, StateBlocked, StateNeedsDecision, StateDone, StateMerging) {
		return fmt.Errorf("%w: %s is %s; wait only on a started, live Task", ErrInvalidTaskDependency, ref, state)
	}
	var cycle bool
	err = tx.QueryRowContext(ctx, `WITH RECURSIVE prerequisites(id) AS (
		VALUES (?)
		UNION
		SELECT w.dependency_id FROM task_dependency_waits w JOIN prerequisites p ON w.task_id=p.id
	) SELECT EXISTS(SELECT 1 FROM prerequisites WHERE id=?)`, id, task.ID).Scan(&cycle)
	if err != nil {
		return err
	}
	if cycle {
		return fmt.Errorf("%w: waiting on %s would create a dependency cycle", ErrInvalidTaskDependency, ref)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_dependency_waits(task_id,dependency_id) VALUES(?,?)`, task.ID, id)
	return err
}

func refuseDependencyWait(ctx context.Context, tx *writeTx, taskID int64) error {
	var waiting bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_dependency_waits WHERE task_id=?)`, taskID).Scan(&waiting); err != nil {
		return err
	}
	if waiting {
		return fmt.Errorf("%w: Task %d is waiting for a dependency to land", ErrStateRace, taskID)
	}
	return nil
}

type dependencyWait struct {
	id        int64
	seq       int
	state     State
	landedRef string
}

func (db *DB) dependencyWait(ctx context.Context, taskID int64) (dependencyWait, error) {
	var wait dependencyWait
	err := db.QueryRowContext(ctx, `SELECT d.id,d.seq,d.state,d.landed_ref FROM task_dependency_waits w
		JOIN tasks d ON d.id=w.dependency_id WHERE w.task_id=?`, taskID).Scan(&wait.id, &wait.seq, &wait.state, &wait.landedRef)
	if errors.Is(err, sql.ErrNoRows) {
		return dependencyWait{}, nil
	}
	return wait, err
}

func (db *DB) TaskDependencyWait(ctx context.Context, taskID int64) (string, error) {
	wait, err := db.dependencyWait(ctx, taskID)
	if err != nil || wait.id == 0 {
		return "", err
	}
	return fmt.Sprintf("t%d:landed (predecessor %s)", wait.seq, wait.state), nil
}

func (db *DB) ReconcileDependencyWait(ctx context.Context, taskID int64) (bool, error) {
	wait, err := db.dependencyWait(ctx, taskID)
	if err != nil || wait.id == 0 {
		return false, err
	}
	if !taskDependencyLanded(wait.state, wait.landedRef) {
		return true, nil
	}
	return true, db.resumeLandedDependencyWait(ctx, taskID)
}

func (db *DB) resumeLandedDependencyWait(ctx context.Context, taskID int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq int
	var state State
	var landedRef string
	err = tx.QueryRowContext(ctx, `SELECT d.seq,d.state,d.landed_ref FROM task_dependency_waits w
		JOIN tasks d ON d.id=w.dependency_id WHERE w.task_id=?
		AND NOT EXISTS(SELECT 1 FROM intents WHERE task_id=w.task_id)`, taskID).Scan(&seq, &state, &landedRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !taskDependencyLanded(state, landedRef) {
		return nil
	}
	note := fmt.Sprintf("Dependency t%d landed", seq)
	if err := transitionTx(ctx, db.queries.WithTx(tx.Tx), taskID, StateBlocked, StateWorking, "watcher", note); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(task_id,body,created_at,status,wait_for_idle) VALUES(?,?,?,'queued',1)`, taskID, note+". Resume work and follow the Task's existing verification and completion steps.", time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}
