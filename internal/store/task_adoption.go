package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrTaskAdoptionChanged = errors.New("adoption source custody or branch head changed")

type TaskAdoptionRequest struct {
	SourceTaskID          int64
	Repository            string
	Branch                string
	PRURL                 string
	ExpectedHead          string
	ExpectedGeneration    int64
	PreviousWriterStopped bool
}

type TaskAdoption struct {
	TaskID       int64  `toml:"task_id"`
	SourceTaskID int64  `toml:"source_task_id"`
	Repository   string `toml:"repository"`
	Branch       string `toml:"branch"`
	PRURL        string `toml:"pr_url"`
	HeadSHA      string `toml:"head_sha"`
	Generation   int64  `toml:"generation"`
	CreatedAt    int64  `toml:"created_at"`
}

func (db *DB) TaskAdoptions(ctx context.Context, taskID int64) ([]TaskAdoption, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id,source_task_id,repository,branch,pr_url,head_sha,generation,created_at FROM task_adoptions WHERE task_id=? OR source_task_id=? ORDER BY created_at,task_id`, taskID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TaskAdoption
	for rows.Next() {
		var a TaskAdoption
		if err := rows.Scan(&a.TaskID, &a.SourceTaskID, &a.Repository, &a.Branch, &a.PRURL, &a.HeadSHA, &a.Generation, &a.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func adoptTaskTx(ctx context.Context, tx *writeTx, projectID, id int64, task Task) error {
	a := task.Adoption
	if task.Type != "change" || task.State == StateQueued || a.SourceTaskID == id || a.PRURL == "" || a.Repository == "" || a.Branch != task.Branch || !validValidationSHA(a.ExpectedHead) || a.ExpectedGeneration < 1 || !a.PreviousWriterStopped {
		return ErrTaskAdoptionChanged
	}
	var sourceState, sourcePR, branch string
	var mountID int64
	var path string
	if err := tx.QueryRowContext(ctx, `SELECT state,pr_url,branch,COALESCE(mount_id,0),worktree_path FROM tasks WHERE id=? AND project_id=? AND type='change'`, a.SourceTaskID, projectID).Scan(&sourceState, &sourcePR, &branch, &mountID, &path); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTaskAdoptionChanged
		}
		return err
	}
	if sourcePR != a.PRURL || branch != a.Branch || sourceState == "merging" || sourceState == "merged" {
		return ErrTaskAdoptionChanged
	}
	var blocked bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decisions WHERE task_id=? AND answered_at=0) OR EXISTS(SELECT 1 FROM task_adoptions WHERE source_task_id=?) OR EXISTS(SELECT 1 FROM intents WHERE task_id=?) OR EXISTS(SELECT 1 FROM delivery_operations o JOIN deliveries d ON d.id=o.delivery_id WHERE d.task_id=? AND o.outcome IN ('pending','uncertain'))`, a.SourceTaskID, a.SourceTaskID, a.SourceTaskID, a.SourceTaskID).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return ErrTaskAdoptionChanged
	}
	result, err := tx.ExecContext(ctx, `UPDATE branch_authorship SET task_id=?,attempt=1,generation=generation+1,updated_at=? WHERE repository=? AND branch=? AND task_id=? AND generation=? AND head_sha=?`, id, time.Now().UnixMilli(), a.Repository, a.Branch, a.SourceTaskID, a.ExpectedGeneration, a.ExpectedHead)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTaskAdoptionChanged
	}
	if err := invalidateBranchAuthorEvidenceWithObservationsTx(ctx, tx, a.SourceTaskID, a.ExpectedHead, true); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_adoptions(task_id,source_task_id,repository,branch,pr_url,head_sha,generation,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, a.SourceTaskID, a.Repository, a.Branch, a.PRURL, a.ExpectedHead, a.ExpectedGeneration+1, now); err != nil {
		return err
	}
	if mountID != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE resource_leases SET owner_task_id=?,owner=(SELECT p.name || ' sandbox-' || m.n || ' (t' || t.seq || ')' FROM tasks t JOIN projects p ON p.id=t.project_id JOIN mounts m ON m.id=? WHERE t.id=?),intent_id=0 WHERE mount_id=? AND owner_task_id=?`, id, mountID, id, mountID, a.SourceTaskID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE mounts SET task_id=? WHERE id=? AND task_id=? AND state='held'`, id, mountID, a.SourceTaskID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("source Sandbox ownership changed: %w", ErrTaskAdoptionChanged)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET pr_url=?,mount_id=?,worktree_path=? WHERE id=?`, a.PRURL, nullableID(mountID), path, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='torn-down',pr_url='',mount_id=NULL,updated_at=? WHERE id=?`, now, a.SourceTaskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES(? ,?,'torn-down','cli',?,?)`, a.SourceTaskID, sourceState, fmt.Sprintf("PR custody adopted by Task %d", id), now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deliveries(task_id,repo,pr_url,steward,origin,created_at) VALUES(?,'',?,'lead','publish',?)`, id, a.PRURL, now)
	return err
}
