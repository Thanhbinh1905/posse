package store

import (
	"context"
	"database/sql"
	"errors"
)

// MergeQueuePlace is one Task's place in the FIFO merge queue of a target.
// Position 1 is the head: the only entry Posse asks to merge the target.
type MergeQueuePlace struct {
	Repo         string
	TargetBranch string
	Position     int
	Length       int
	HeadTaskID   int64
	HeadSeq      int
}

// Queued reports whether the Task holds a live entry in its target's queue.
func (place MergeQueuePlace) Queued() bool { return place.Position > 0 }

// Head reports whether the Task is the head of its target's queue.
func (place MergeQueuePlace) Head() bool { return place.Position == 1 }

type mergeQueueQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// A freshness instruction is in flight from the moment it is queued until the
// Worker's next done Signal. Its Task is the head even while working.
const mergeQueueInFlightSQL = `EXISTS(SELECT 1 FROM validation_fix_rounds r JOIN messages m ON m.id=r.message_id
WHERE r.ship_task_id=e.task_id AND r.kind='freshness'
AND substr(r.dedupe_key,1,length('freshness:'||e.repo||':'))='freshness:'||e.repo||':'
AND m.status IN ('queued','claimed','interrupting','submitting','delivered')
AND m.created_at>COALESCE((SELECT MAX(at) FROM signals WHERE task_id=e.task_id AND verb='done'),0))`

const mergeQueueLiveSQL = `(t.state IN ('done','merging') OR t.state='working' AND ` + mergeQueueInFlightSQL + `)`

// EnterMergeQueue adds a done or merging Change Task to the FIFO queue of
// one target unless it already holds a live entry there. Entries order by
// the Task's latest done Signal.
func (db *DB) EnterMergeQueue(ctx context.Context, taskID int64, repo, targetBranch string, now int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := enterMergeQueueTx(ctx, tx, taskID, repo, targetBranch, now); err != nil {
		return err
	}
	return tx.Commit()
}

func enterMergeQueueTx(ctx context.Context, tx *writeTx, taskID int64, repo, targetBranch string, now int64) error {
	var projectID int64
	var state, taskType string
	var doneAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT project_id,state,type,(SELECT MAX(at) FROM signals WHERE task_id=tasks.id AND verb='done') FROM tasks WHERE id=?`, taskID).Scan(&projectID, &state, &taskType, &doneAt); err != nil {
		return err
	}
	if taskType != "change" || !doneAt.Valid {
		return nil
	}
	var liveTarget string
	err := tx.QueryRowContext(ctx, `SELECT target_branch FROM merge_queue_entries WHERE task_id=? AND repo=? AND dequeued_at=0`, taskID, repo).Scan(&liveTarget)
	if err == nil {
		if liveTarget != targetBranch {
			_, err = tx.ExecContext(ctx, `UPDATE merge_queue_entries SET target_branch=? WHERE task_id=? AND repo=? AND dequeued_at=0`, targetBranch, taskID, repo)
		}
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if State(state) != StateDone && State(state) != StateMerging {
		return nil
	}
	// A dequeued Task stays out until a later done Signal or green checks,
	// and then re-enters at the back rather than at its done Signal.
	var lastDequeue, lastGreen int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(dequeued_at),0) FROM merge_queue_entries WHERE task_id=? AND repo=? AND dequeued_at>=?`, taskID, repo, doneAt.Int64).Scan(&lastDequeue); err != nil {
		return err
	}
	enteredAt := doneAt.Int64
	if lastDequeue > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(observed_at),0) FROM pr_observations WHERE task_id=? AND repo=? AND json_extract(checks,'$.state')='SUCCESS'`, taskID, repo).Scan(&lastGreen); err != nil {
			return err
		}
		if lastGreen <= lastDequeue {
			return nil
		}
		enteredAt = now
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO merge_queue_entries(project_id,task_id,repo,target_branch,entered_at,created_at) VALUES(?,?,?,?,?,?)`, projectID, taskID, repo, targetBranch, enteredAt, now)
	return err
}

// PlaceInMergeQueue enters the Task, dequeues entries whose Tasks left done
// or merging without an in-flight freshness instruction, and returns the
// Task's place. A zero Position means the Task is not queued.
func (db *DB) PlaceInMergeQueue(ctx context.Context, taskID int64, repo, targetBranch string, now int64) (MergeQueuePlace, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return MergeQueuePlace{}, err
	}
	defer tx.Rollback()
	if err := enterMergeQueueTx(ctx, tx, taskID, repo, targetBranch, now); err != nil {
		return MergeQueuePlace{}, err
	}
	var projectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=?`, taskID).Scan(&projectID); err != nil {
		return MergeQueuePlace{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE merge_queue_entries SET dequeued_at=?,dequeue_reason='left_done'
WHERE id IN (SELECT e.id FROM merge_queue_entries e JOIN tasks t ON t.id=e.task_id
WHERE e.project_id=? AND e.repo=? AND e.target_branch=? AND e.dequeued_at=0 AND NOT `+mergeQueueLiveSQL+`)`, now, projectID, repo, targetBranch); err != nil {
		return MergeQueuePlace{}, err
	}
	places, err := mergeQueuePlaces(ctx, tx, projectID, repo, targetBranch)
	if err != nil {
		return MergeQueuePlace{}, err
	}
	if err := tx.Commit(); err != nil {
		return MergeQueuePlace{}, err
	}
	return places[taskID], nil
}

// DequeueMergeQueue removes a Task's live entry for one repository, for
// example after its PR merged or its fresh head failed required checks. The
// Task re-enters only after a later done Signal.
func (db *DB) DequeueMergeQueue(ctx context.Context, taskID int64, repo, reason string, now int64) error {
	_, err := db.ExecContext(ctx, `UPDATE merge_queue_entries SET dequeued_at=?,dequeue_reason=? WHERE task_id=? AND repo=? AND dequeued_at=0`, now, reason, taskID, repo)
	return err
}

// MergeQueuePlaces reports every live queue place of a Project's Tasks
// without changing the queue, keyed by Task ID.
func (db *DB) MergeQueuePlaces(ctx context.Context, projectID int64) (map[int64][]MergeQueuePlace, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT repo,target_branch FROM merge_queue_entries WHERE project_id=? AND dequeued_at=0 ORDER BY repo,target_branch`, projectID)
	if err != nil {
		return nil, err
	}
	type queueKey struct{ repo, target string }
	keys := []queueKey{}
	for rows.Next() {
		var key queueKey
		if err := rows.Scan(&key.repo, &key.target); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := map[int64][]MergeQueuePlace{}
	for _, key := range keys {
		places, err := mergeQueuePlaces(ctx, db, projectID, key.repo, key.target)
		if err != nil {
			return nil, err
		}
		for taskID, place := range places {
			result[taskID] = append(result[taskID], place)
		}
	}
	return result, nil
}

// mergeQueuePlaces ranks live entries: an entry with an in-flight freshness
// instruction stays the head, then entries follow their done Signal order.
func mergeQueuePlaces(ctx context.Context, queryer mergeQueueQueryer, projectID int64, repo, targetBranch string) (map[int64]MergeQueuePlace, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT e.task_id,t.seq FROM merge_queue_entries e JOIN tasks t ON t.id=e.task_id
WHERE e.project_id=? AND e.repo=? AND e.target_branch=? AND e.dequeued_at=0 AND `+mergeQueueLiveSQL+`
ORDER BY `+mergeQueueInFlightSQL+` DESC,e.entered_at,e.id`, projectID, repo, targetBranch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type entry struct {
		taskID int64
		seq    int
	}
	entries := []entry{}
	for rows.Next() {
		var current entry
		if err := rows.Scan(&current.taskID, &current.seq); err != nil {
			return nil, err
		}
		entries = append(entries, current)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	places := make(map[int64]MergeQueuePlace, len(entries))
	for index, current := range entries {
		places[current.taskID] = MergeQueuePlace{Repo: repo, TargetBranch: targetBranch, Position: index + 1, Length: len(entries), HeadTaskID: entries[0].taskID, HeadSeq: entries[0].seq}
	}
	return places, nil
}
