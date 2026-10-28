package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrReviewRoundActive         = errors.New("review round is already active")
	ErrReviewRoundUnchanged      = errors.New("change head is unchanged since its last review")
	ErrReviewRoundCap            = errors.New("review round cap reached")
	ErrReviewRoundNumber         = errors.New("review round number changed concurrently")
	ErrReviewResolutionsRequired = errors.New("review fix resolutions are required")
	ErrReviewResolutionsInvalid  = errors.New("review fix resolutions are invalid")
)

// ReviewRoundAdmission binds a new Review Task to one numbered round. All
// repository bindings are inserted with the Task in the same transaction.
type ReviewRoundAdmission struct {
	ChangeTaskID       int64
	Round              int
	MaxRounds          int
	SharedRepairBudget bool
	Bindings           []ReviewRound
}

type ReviewLedgerEntry struct {
	FindingSeq  int
	PatternSeq  int
	PatternKey  string
	State       string
	Severity    string
	Fingerprint string
}

// ReviewLedgerFor returns the Change Task's current finding ledger in stable ID order.
func (db *DB) ReviewLedgerFor(ctx context.Context, changeTaskID int64) ([]ReviewLedgerEntry, error) {
	rows, err := db.QueryContext(ctx, `SELECT f.seq,COALESCE(parent.seq,p.seq),COALESCE(parent.key,p.key),CASE WHEN f.inherited_from IS NOT NULL THEN 'inherited' ELSE f.state END,f.severity,f.fingerprint FROM review_findings f JOIN review_patterns p ON p.id=f.pattern_id LEFT JOIN review_patterns parent ON parent.id=p.merged_into WHERE f.ship_task_id=? ORDER BY f.seq`, changeTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []ReviewLedgerEntry
	for rows.Next() {
		var entry ReviewLedgerEntry
		if err := rows.Scan(&entry.FindingSeq, &entry.PatternSeq, &entry.PatternKey, &entry.State, &entry.Severity, &entry.Fingerprint); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ReviewRoundsFor returns the immutable bindings and current status for every
// round of a Change Task.
func (db *DB) ReviewRoundsFor(ctx context.Context, changeTaskID int64) ([]ReviewRound, error) {
	rows, err := db.QueryContext(ctx, `SELECT ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,COALESCE(verdict,''),created_at FROM review_rounds WHERE ship_task_id=? ORDER BY round,repository,review_task_id`, changeTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rounds []ReviewRound
	for rows.Next() {
		var round ReviewRound
		if err := rows.Scan(&round.ChangeTaskID, &round.Round, &round.ReviewTaskID, &round.Repository, &round.ScopeKind, &round.ScopeBaseSHA, &round.HeadSHA, &round.TargetCommitSHA, &round.BaselineTree, &round.DeltaSHA256, &round.ConflictedPaths, &round.Status, &round.Verdict, &round.CreatedAt); err != nil {
			return nil, err
		}
		rounds = append(rounds, round)
	}
	return rounds, rows.Err()
}

func (db *DB) ReviewTargetsCovered(ctx context.Context, changeTaskID int64, targets []ReviewTarget) (bool, error) {
	for _, target := range targets {
		var covered bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM review_rounds WHERE ship_task_id=? AND repository=? AND status='verdict' AND head_sha=?
UNION ALL
SELECT 1 FROM review_carries WHERE ship_task_id=? AND repository=? AND to_head=?)`,
			changeTaskID, target.Repository, target.Commit,
			changeTaskID, target.Repository, target.Commit).Scan(&covered); err != nil || !covered {
			return false, err
		}
	}
	return len(targets) > 0, nil
}

func validateReviewRoundAdmissionTx(ctx context.Context, tx *writeTx, admission ReviewRoundAdmission) error {
	if admission.ChangeTaskID < 1 || admission.Round < 1 || admission.MaxRounds < 1 || admission.MaxRounds > 3 || len(admission.Bindings) == 0 {
		return fmt.Errorf("invalid review round admission")
	}
	var taskType string
	if err := tx.QueryRowContext(ctx, `SELECT type FROM tasks WHERE id=?`, admission.ChangeTaskID).Scan(&taskType); err != nil {
		return err
	}
	if taskType != "change" {
		return fmt.Errorf("review round target is not a Change Task")
	}
	// A completed Review Task without a recorded verdict is a void round. Settle
	// it here as well as on admission so crashes and older transitions cannot
	// leave the active-round guard stuck indefinitely.
	if _, err := tx.ExecContext(ctx, `UPDATE review_rounds SET status='void'
WHERE ship_task_id=? AND status='admitted'
  AND EXISTS (SELECT 1 FROM tasks r WHERE r.id=review_rounds.review_task_id AND r.state IN ('failed','lost','reported','torn-down'))
  AND NOT EXISTS (SELECT 1 FROM review_verdicts v WHERE v.task_id=review_rounds.review_task_id)`, admission.ChangeTaskID); err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM review_rounds rr JOIN tasks r ON r.id=rr.review_task_id
WHERE rr.ship_task_id=? AND rr.status='admitted'
  AND r.state NOT IN ('failed','lost','reported','torn-down'))`, admission.ChangeTaskID).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrReviewRoundActive
	}
	var used, repairs int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT round) FROM review_rounds WHERE ship_task_id=? AND status='verdict'`, admission.ChangeTaskID).Scan(&used); err != nil {
		return err
	}
	if admission.SharedRepairBudget {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM validation_fix_rounds WHERE ship_task_id=? AND kind IN ('review','ci')`, admission.ChangeTaskID).Scan(&repairs); err != nil {
			return err
		}
	}
	if admission.Round != used+1 {
		return ErrReviewRoundNumber
	}
	seen := make(map[string]bool, len(admission.Bindings))
	anyNewHead := false
	for _, binding := range admission.Bindings {
		if binding.ChangeTaskID != admission.ChangeTaskID || binding.Round != admission.Round || binding.Repository == "" || seen[binding.Repository] || binding.Status != "admitted" || binding.Verdict != "" {
			return fmt.Errorf("invalid repository binding in review round admission")
		}
		seen[binding.Repository] = true
		var unchanged bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM review_rounds WHERE ship_task_id=? AND repository=? AND status='verdict' AND head_sha=?
UNION ALL
SELECT 1 FROM review_carries WHERE ship_task_id=? AND repository=? AND to_head=?)`,
			admission.ChangeTaskID, binding.Repository, binding.HeadSHA,
			admission.ChangeTaskID, binding.Repository, binding.HeadSHA).Scan(&unchanged); err != nil {
			return err
		}
		if !unchanged {
			anyNewHead = true
		}
	}
	if !anyNewHead {
		return ErrReviewRoundUnchanged
	}
	if used+repairs >= admission.MaxRounds {
		return ErrReviewRoundCap
	}
	return nil
}

func insertReviewRoundBindingsTx(ctx context.Context, tx *writeTx, reviewTaskID int64, admission ReviewRoundAdmission) error {
	createdAt := time.Now().UnixMilli()
	for _, binding := range admission.Bindings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,'admitted',NULL,?)`,
			admission.ChangeTaskID, admission.Round, reviewTaskID, binding.Repository, binding.ScopeKind, binding.ScopeBaseSHA, binding.HeadSHA, binding.TargetCommitSHA, binding.BaselineTree, binding.DeltaSHA256, binding.ConflictedPaths, createdAt); err != nil {
			return err
		}
	}
	return nil
}
