package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrBranchAuthorWriterActive = errors.New("previous branch author is still active")
	ErrBranchAuthorHeadChanged  = errors.New("branch head changed since the previous author claim")
	ErrBranchAuthorStale        = errors.New("branch author claim is stale")
)

type BranchAuthorClaim struct {
	Repository string
	Branch     string
	TaskID     int64
	Attempt    int
	Generation int64
	HeadSHA    string
}

// ClaimBranchAuthor transfers branch authorship after the caller has stopped
// the prior Worker and verified the current external head.
func (db *DB) ClaimBranchAuthor(ctx context.Context, repository, branch string, taskID int64, attempt int, previousHead, nextHead string, previousWriterStopped bool) (BranchAuthorClaim, error) {
	repository, branch = strings.TrimSpace(repository), strings.TrimSpace(branch)
	previousHead, nextHead = strings.TrimSpace(previousHead), strings.TrimSpace(nextHead)
	if repository == "" || branch == "" || taskID < 1 || attempt < 1 || !validBranchAuthorHead(previousHead) || !validBranchAuthorHead(nextHead) {
		return BranchAuthorClaim{}, fmt.Errorf("repository, branch, task, attempt and valid heads are required")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return BranchAuthorClaim{}, err
	}
	defer tx.Rollback()
	var superseded bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_adoptions WHERE source_task_id=?)`, taskID).Scan(&superseded); err != nil {
		return BranchAuthorClaim{}, err
	}
	if superseded {
		return BranchAuthorClaim{}, ErrBranchAuthorStale
	}
	claim := BranchAuthorClaim{Repository: repository, Branch: branch, TaskID: taskID, Attempt: attempt, HeadSHA: nextHead}
	var ownerTask int64
	var ownerAttempt int
	var generation int64
	var head string
	err = tx.QueryRowContext(ctx, `SELECT task_id,attempt,generation,head_sha FROM branch_authorship WHERE repository=? AND branch=?`, repository, branch).Scan(&ownerTask, &ownerAttempt, &generation, &head)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if previousHead != nextHead {
			return BranchAuthorClaim{}, ErrBranchAuthorHeadChanged
		}
		claim.Generation = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO branch_authorship(repository,branch,task_id,attempt,generation,head_sha,updated_at) VALUES(?,?,?,?,?,?,?)`, repository, branch, taskID, attempt, claim.Generation, nextHead, time.Now().UnixMilli())
	case err != nil:
		return BranchAuthorClaim{}, err
	case !previousWriterStopped:
		return BranchAuthorClaim{}, ErrBranchAuthorWriterActive
	case head != previousHead:
		return BranchAuthorClaim{}, ErrBranchAuthorHeadChanged
	default:
		if ownerTask != taskID || head != nextHead {
			if err := invalidateBranchAuthorEvidenceTx(ctx, tx, ownerTask, nextHead); err != nil {
				return BranchAuthorClaim{}, err
			}
		}
		claim.Generation = generation + 1
		_, err = tx.ExecContext(ctx, `UPDATE branch_authorship SET task_id=?,attempt=?,generation=?,head_sha=?,updated_at=? WHERE repository=? AND branch=? AND generation=? AND head_sha=?`, taskID, attempt, claim.Generation, nextHead, time.Now().UnixMilli(), repository, branch, generation, previousHead)
	}
	if err != nil {
		return BranchAuthorClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return BranchAuthorClaim{}, err
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		return BranchAuthorClaim{}, err
	}
	if ownerTask > 0 && ownerTask != taskID {
		if err := db.PersistTask(ctx, ownerTask); err != nil {
			return BranchAuthorClaim{}, err
		}
	}
	return claim, nil
}

func invalidateBranchAuthorEvidenceTx(ctx context.Context, tx *writeTx, taskID int64, head string) error {
	return invalidateBranchAuthorEvidenceWithObservationsTx(ctx, tx, taskID, head, false)
}

func invalidateBranchAuthorEvidenceWithObservationsTx(ctx context.Context, tx *writeTx, taskID int64, head string, preserveObservations bool) error {
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET gated_sha='',updated_at=? WHERE id=?`, now, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_repos SET gated_sha='',state=CASE WHEN state IN ('gated','unchanged','merging') THEN 'open' ELSE state END,updated_at=? WHERE task_id=? AND state<>'merged'`, now, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO branch_author_invalidated_verdicts(task_id,change_task_id,invalidated_at) SELECT v.task_id,t.reviews_task_id,? FROM review_verdicts v JOIN tasks t ON t.id=v.task_id WHERE t.reviews_task_id=?`, now, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_rounds SET status='void',verdict=NULL WHERE ship_task_id=? AND status IN ('admitted','verdict')`, taskID); err != nil {
		return err
	}
	if !preserveObservations {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pr_observations WHERE task_id=?`, taskID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE validation_candidates SET state='stale',updated_at=? WHERE task_id=? AND state IN ('preparing','active','promoting')`, now, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE validation_stage_results SET status='invalidated',invalidated_by_sha=? WHERE task_id=? AND status IN ('passed','failed')`, head, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM approvals WHERE task_id=?`, taskID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE merge_queue_entries SET dequeued_at=?,dequeue_reason='author_transferred' WHERE task_id=? AND dequeued_at=0`, now, taskID); err != nil {
		return err
	}
	return nil
}

func (db *DB) InvalidatedReviewVerdictsForChange(ctx context.Context, changeTaskID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT task_id FROM branch_author_invalidated_verdicts WHERE change_task_id=? ORDER BY task_id`, changeTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var taskIDs []int64
	for rows.Next() {
		var taskID int64
		if err := rows.Scan(&taskID); err != nil {
			return nil, err
		}
		taskIDs = append(taskIDs, taskID)
	}
	return taskIDs, rows.Err()
}

func (db *DB) BranchAuthorsForTask(ctx context.Context, taskID int64) ([]BranchAuthorClaim, error) {
	rows, err := db.QueryContext(ctx, `SELECT repository,branch,task_id,attempt,generation,head_sha FROM branch_authorship WHERE task_id=? ORDER BY repository,branch`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []BranchAuthorClaim
	for rows.Next() {
		var claim BranchAuthorClaim
		if err := rows.Scan(&claim.Repository, &claim.Branch, &claim.TaskID, &claim.Attempt, &claim.Generation, &claim.HeadSHA); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (db *DB) BranchAuthor(ctx context.Context, repository, branch string) (BranchAuthorClaim, error) {
	var claim BranchAuthorClaim
	err := db.QueryRowContext(ctx, `SELECT repository,branch,task_id,attempt,generation,head_sha FROM branch_authorship WHERE repository=? AND branch=?`, repository, branch).Scan(
		&claim.Repository, &claim.Branch, &claim.TaskID, &claim.Attempt, &claim.Generation, &claim.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return BranchAuthorClaim{}, ErrNotFound
	}
	return claim, err
}

func (db *DB) RequireBranchAuthor(ctx context.Context, repository, branch string, taskID int64, generation int64) error {
	if repository == "" || branch == "" || taskID < 1 || generation < 1 {
		return ErrBranchAuthorStale
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM branch_authorship WHERE repository=? AND branch=? AND task_id=? AND generation=?)`, repository, branch, taskID, generation).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrBranchAuthorStale
	}
	return nil
}

func validBranchAuthorHead(head string) bool {
	return head == "" || validValidationSHA(head)
}

func (db *DB) UpdateBranchAuthorHead(ctx context.Context, repository, branch string, taskID int64, generation int64, expectedHead, nextHead string) error {
	if !validBranchAuthorHead(expectedHead) || !validValidationSHA(nextHead) {
		return ErrBranchAuthorHeadChanged
	}
	result, err := db.ExecContext(ctx, `UPDATE branch_authorship SET head_sha=?,updated_at=? WHERE repository=? AND branch=? AND task_id=? AND generation=? AND head_sha=?`, nextHead, time.Now().UnixMilli(), repository, branch, taskID, generation, expectedHead)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrBranchAuthorStale
	}
	return db.PersistTask(ctx, taskID)
}
