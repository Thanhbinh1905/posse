package store

import (
	"context"
	"fmt"
	"time"
)

const (
	ValidationCandidatePreparing  = "preparing"
	ValidationCandidateActive     = "active"
	ValidationCandidateStale      = "stale"
	ValidationCandidatePromoting  = "promoting"
	ValidationCandidatePromoted   = "promoted"
	ValidationCandidateSuperseded = "superseded"
	ValidationCandidateDiscarded  = "discarded"
)

// ValidationCandidate durably binds an author submission to its private
// validation ref, target and expected published branch head.
type ValidationCandidate struct {
	ID                int64  `toml:"id"`
	TaskID            int64  `toml:"task_id"`
	Repo              string `toml:"repo"`
	Generation        int    `toml:"generation"`
	SubmittedSHA      string `toml:"submitted_sha"`
	CandidateSHA      string `toml:"candidate_sha"`
	TargetSHA         string `toml:"target_sha"`
	ExpectedRemoteSHA string `toml:"expected_remote_sha"`
	ObservedAuthorSHA string `toml:"observed_author_sha,omitempty"`
	RefName           string `toml:"ref_name"`
	WorktreePath      string `toml:"worktree_path"`
	State             string `toml:"state"`
	PromotedSHA       string `toml:"promoted_sha,omitempty"`
	Summary           string `toml:"summary"`
	Verification      string `toml:"verification,omitempty"`
	Proof             string `toml:"proof,omitempty"`
	Risk              string `toml:"risk,omitempty"`
	Refresh           bool   `toml:"refresh,omitempty"`
	CreatedAt         int64  `toml:"created_at"`
	UpdatedAt         int64  `toml:"updated_at"`
}

func (db *DB) ValidationCandidates(ctx context.Context, taskID int64) ([]ValidationCandidate, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,task_id,repo,generation,submitted_sha,candidate_sha,target_sha,expected_remote_sha,observed_author_sha,ref_name,worktree_path,state,promoted_sha,summary,verification,proof,risk,refresh,created_at,updated_at FROM validation_candidates WHERE task_id=? ORDER BY repo,generation`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []ValidationCandidate
	for rows.Next() {
		candidate, err := scanValidationCandidate(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (db *DB) NextValidationCandidateGeneration(ctx context.Context, taskID int64, repo string) (int, error) {
	var generation int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM validation_candidates WHERE task_id=? AND repo=?`, taskID, repo).Scan(&generation)
	return generation, err
}

func (db *DB) LatestValidationCandidate(ctx context.Context, taskID int64, repo string) (ValidationCandidate, error) {
	candidate, err := scanValidationCandidate(db.QueryRowContext(ctx, `SELECT id,task_id,repo,generation,submitted_sha,candidate_sha,target_sha,expected_remote_sha,observed_author_sha,ref_name,worktree_path,state,promoted_sha,summary,verification,proof,risk,refresh,created_at,updated_at FROM validation_candidates WHERE task_id=? AND repo=? AND state<>'discarded' ORDER BY generation DESC LIMIT 1`, taskID, repo))
	if IsNotFound(err) {
		return ValidationCandidate{}, ErrNotFound
	}
	return candidate, err
}

// PromotedValidationCandidateBefore returns the newest candidate older than
// generation that was promoted, which an abandonment returns the Task to.
func (db *DB) PromotedValidationCandidateBefore(ctx context.Context, taskID int64, repo string, generation int) (ValidationCandidate, error) {
	candidate, err := scanValidationCandidate(db.QueryRowContext(ctx, `SELECT id,task_id,repo,generation,submitted_sha,candidate_sha,target_sha,expected_remote_sha,observed_author_sha,ref_name,worktree_path,state,promoted_sha,summary,verification,proof,risk,refresh,created_at,updated_at FROM validation_candidates WHERE task_id=? AND repo=? AND generation<? AND state IN ('promoted','superseded') AND promoted_sha<>'' ORDER BY generation DESC LIMIT 1`, taskID, repo, generation))
	if IsNotFound(err) {
		return ValidationCandidate{}, ErrNotFound
	}
	return candidate, err
}

func (db *DB) LiveValidationCandidates(ctx context.Context, taskID int64) ([]ValidationCandidate, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,task_id,repo,generation,submitted_sha,candidate_sha,target_sha,expected_remote_sha,observed_author_sha,ref_name,worktree_path,state,promoted_sha,summary,verification,proof,risk,refresh,created_at,updated_at FROM validation_candidates WHERE task_id=? AND state IN ('preparing','active','stale','promoting') ORDER BY repo,generation`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []ValidationCandidate
	for rows.Next() {
		candidate, err := scanValidationCandidate(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

type validationCandidateScanner interface {
	Scan(dest ...any) error
}

func scanValidationCandidate(row validationCandidateScanner) (ValidationCandidate, error) {
	var candidate ValidationCandidate
	var refresh int
	err := row.Scan(&candidate.ID, &candidate.TaskID, &candidate.Repo, &candidate.Generation,
		&candidate.SubmittedSHA, &candidate.CandidateSHA, &candidate.TargetSHA, &candidate.ExpectedRemoteSHA,
		&candidate.ObservedAuthorSHA, &candidate.RefName, &candidate.WorktreePath, &candidate.State,
		&candidate.PromotedSHA, &candidate.Summary, &candidate.Verification, &candidate.Proof, &candidate.Risk,
		&refresh, &candidate.CreatedAt, &candidate.UpdatedAt)
	candidate.Refresh = refresh != 0
	return candidate, err
}

// CreateValidationCandidate records the immutable submission before Git effects
// begin. A preparing row lets a later publish recover an interrupted checkout.
func (db *DB) CreateValidationCandidate(ctx context.Context, candidate ValidationCandidate) (ValidationCandidate, error) {
	if candidate.TaskID < 1 || candidate.Generation != 0 || candidate.SubmittedSHA == "" || candidate.CandidateSHA != candidate.SubmittedSHA || candidate.TargetSHA == "" || candidate.RefName == "" || candidate.WorktreePath == "" || candidate.Summary == "" {
		return ValidationCandidate{}, fmt.Errorf("invalid validation candidate submission")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return ValidationCandidate{}, err
	}
	defer tx.Rollback()
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_candidates WHERE task_id=? AND repo=? AND state IN ('preparing','active','stale','promoting')`, candidate.TaskID, candidate.Repo).Scan(&live); err != nil {
		return ValidationCandidate{}, err
	}
	if live != 0 {
		return ValidationCandidate{}, ErrStateRace
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM validation_candidates WHERE task_id=? AND repo=?`, candidate.TaskID, candidate.Repo).Scan(&candidate.Generation); err != nil {
		return ValidationCandidate{}, err
	}
	candidate.State = ValidationCandidatePreparing
	candidate.CreatedAt = time.Now().UnixMilli()
	candidate.UpdatedAt = candidate.CreatedAt
	refresh := 0
	if candidate.Refresh {
		refresh = 1
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO validation_candidates(task_id,repo,generation,submitted_sha,candidate_sha,target_sha,expected_remote_sha,ref_name,worktree_path,state,summary,verification,proof,risk,refresh,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,'preparing',?,?,?,?,?,?,?)`,
		candidate.TaskID, candidate.Repo, candidate.Generation, candidate.SubmittedSHA, candidate.CandidateSHA, candidate.TargetSHA, candidate.ExpectedRemoteSHA,
		candidate.RefName, candidate.WorktreePath, candidate.Summary, candidate.Verification, candidate.Proof, candidate.Risk, refresh, candidate.CreatedAt, candidate.UpdatedAt)
	if err != nil {
		return ValidationCandidate{}, err
	}
	candidate.ID, err = result.LastInsertId()
	if err != nil {
		return ValidationCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return ValidationCandidate{}, err
	}
	if err := db.PersistTask(ctx, candidate.TaskID); err != nil {
		return candidate, err
	}
	return candidate, nil
}

// UpdateValidationCandidateDescription rewrites the PR description fields of a
// candidate that has not been promoted. An empty field keeps its stored value.
func (db *DB) UpdateValidationCandidateDescription(ctx context.Context, id int64, summary, verification, proof, risk string) error {
	result, err := db.ExecContext(ctx, `UPDATE validation_candidates SET
		summary=CASE WHEN ?<>'' THEN ? ELSE summary END,
		verification=CASE WHEN ?<>'' THEN ? ELSE verification END,
		proof=CASE WHEN ?<>'' THEN ? ELSE proof END,
		risk=CASE WHEN ?<>'' THEN ? ELSE risk END,
		updated_at=? WHERE id=? AND state IN ('preparing','active')`,
		summary, summary, verification, verification, proof, proof, risk, risk, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	return nil
}

func (db *DB) ActivateValidationCandidate(ctx context.Context, id int64, candidateSHA string) error {
	result, err := db.ExecContext(ctx, `UPDATE validation_candidates SET state='active',candidate_sha=?,updated_at=? WHERE id=? AND state='preparing' AND candidate_sha=?`, candidateSHA, time.Now().UnixMilli(), id, candidateSHA)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	return db.persistCandidateTask(ctx, id)
}

// AdvanceValidationCandidate records a candidate ref update only if it extends
// the head previously stored by Posse.
func (db *DB) AdvanceValidationCandidate(ctx context.Context, id int64, expectedSHA, candidateSHA string) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updatedAt := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE validation_candidates SET candidate_sha=?,updated_at=? WHERE id=? AND state IN ('preparing','active','stale','promoting') AND candidate_sha=?`, candidateSHA, updatedAt, id, expectedSHA)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	if candidateSHA != expectedSHA {
		if _, err := tx.ExecContext(ctx, `UPDATE validation_stage_results SET status='invalidated',invalidated_by_sha=? WHERE candidate_id=? AND status='passed'`, candidateSHA, id); err != nil {
			return err
		}
	}
	var taskID int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM validation_candidates WHERE id=?`, id).Scan(&taskID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) MarkValidationCandidateStale(ctx context.Context, id int64, observedSHA string) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE validation_candidates SET state='stale',observed_author_sha=?,updated_at=? WHERE id=? AND state IN ('preparing','active','promoting')`, observedSHA, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	if _, err := tx.ExecContext(ctx, `UPDATE validation_stage_results SET status='invalidated',invalidated_by_sha=? WHERE candidate_id=? AND status='passed'`, observedSHA, id); err != nil {
		return err
	}
	var taskID int64
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM validation_candidates WHERE id=?`, id).Scan(&taskID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) SupersedeValidationCandidate(ctx context.Context, id int64, expectedState, archiveRef, candidateSHA string) error {
	if expectedState != ValidationCandidateStale && expectedState != ValidationCandidatePromoted {
		return fmt.Errorf("only stale or promoted candidates can be superseded")
	}
	result, err := db.ExecContext(ctx, `UPDATE validation_candidates SET state='superseded',ref_name=?,candidate_sha=?,updated_at=? WHERE id=? AND state=?`, archiveRef, candidateSHA, time.Now().UnixMilli(), id, expectedState)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	return db.persistCandidateTask(ctx, id)
}

func (db *DB) BeginValidationCandidatePromotion(ctx context.Context, id int64) error {
	result, err := db.ExecContext(ctx, `UPDATE validation_candidates SET state='promoting',updated_at=? WHERE id=? AND state IN ('active','promoting')`, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	return db.persistCandidateTask(ctx, id)
}

func (db *DB) CompleteValidationCandidatePromotion(ctx context.Context, id int64, expectedSHA string) error {
	result, err := db.ExecContext(ctx, `UPDATE validation_candidates SET state='promoted',promoted_sha=candidate_sha,updated_at=? WHERE id=? AND state='promoting' AND candidate_sha=?`, time.Now().UnixMilli(), id, expectedSHA)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	return db.persistCandidateTask(ctx, id)
}

// AbandonValidationCandidate discards a stale, archived candidate and every
// unpromoted candidate after the promoted one, restores the promoted one with
// the Task-private ref and records note in Task history.
func (db *DB) AbandonValidationCandidate(ctx context.Context, abandoned ValidationCandidate, archiveRef, archivedSHA string, promoted ValidationCandidate, privateRef, source, note string) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE validation_candidates SET state='discarded',ref_name=?,candidate_sha=?,updated_at=? WHERE id=? AND task_id=? AND state='stale'`, archiveRef, archivedSHA, now, abandoned.ID, abandoned.TaskID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	if _, err := tx.ExecContext(ctx, `UPDATE validation_candidates SET state='discarded',updated_at=? WHERE task_id=? AND repo=? AND generation>? AND generation<? AND state='superseded'`, now, abandoned.TaskID, abandoned.Repo, promoted.Generation, abandoned.Generation); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE validation_candidates SET state='promoted',ref_name=?,updated_at=? WHERE id=? AND task_id=? AND repo=? AND state IN ('promoted','superseded') AND promoted_sha<>'' AND promoted_sha=candidate_sha`, privateRef, now, promoted.ID, abandoned.TaskID, abandoned.Repo)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrStateRace
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, abandoned.TaskID).Scan(&state); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES(?,?,?,?,?,?)`, abandoned.TaskID, state, state, source, note, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, abandoned.TaskID)
}

func (db *DB) persistCandidateTask(ctx context.Context, candidateID int64) error {
	var taskID int64
	if err := db.QueryRowContext(ctx, `SELECT task_id FROM validation_candidates WHERE id=?`, candidateID).Scan(&taskID); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}
