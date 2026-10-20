package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Review verdicts. Only ReviewPass satisfies quick review.
const (
	ReviewPass             = "pass"
	ReviewChangesRequested = "changes_requested"
	ReviewInconclusive     = "inconclusive"
)

var (
	// ErrReviewTargetUnbound means a Review Task has no recorded target, so no
	// verdict can qualify for it.
	ErrReviewTargetUnbound = errors.New("review task has no bound target")
	ErrReviewVerdictType   = errors.New("verdicts are valid only for Review Tasks")
	ErrReviewVerdictFound  = errors.New("review verdict already recorded")
)

// ReviewTarget pins one repository of a completed Change candidate to a commit.
// Repository is the Project name for a repository Project and the Member name
// for a workspace Project.
type ReviewTarget struct {
	Repository string `toml:"repository" json:"repository"`
	Commit     string `toml:"commit" json:"commit"`
}

// ReviewVerdict is the typed outcome of one Review Task. Posse supplies every
// field except Verdict and the Report digest from its own records.
type ReviewVerdict struct {
	TaskID       int64              `toml:"task_id" json:"task_id"`
	SignalID     int64              `toml:"signal_id" json:"signal_id"`
	Verdict      string             `toml:"verdict" json:"verdict"`
	ReportSHA256 string             `toml:"report_sha256" json:"report_sha256"`
	Targets      []ReviewTarget     `toml:"targets" json:"targets"`
	Reviewer     TaskLaunchIdentity `toml:"reviewer" json:"reviewer"`
	CreatedAt    int64              `toml:"created_at" json:"created_at"`
}

// ReviewVerdictInput combines the Worker's verdict with Posse-validated report and findings evidence.
type ReviewVerdictInput struct {
	Verdict      string
	ReportSHA256 string
	Findings     *ReviewFindingsInput
}

func ValidReviewVerdict(verdict string) bool {
	return verdict == ReviewPass || verdict == ReviewChangesRequested || verdict == ReviewInconclusive
}

func sortReviewTargets(targets []ReviewTarget) {
	sort.Slice(targets, func(i, j int) bool { return targets[i].Repository < targets[j].Repository })
}

// SameReviewTargets reports whether two target sets pin the same commits.
func SameReviewTargets(left, right []ReviewTarget) bool {
	if len(left) != len(right) || len(left) == 0 {
		return false
	}
	a := append([]ReviewTarget(nil), left...)
	b := append([]ReviewTarget(nil), right...)
	sortReviewTargets(a)
	sortReviewTargets(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func insertReviewTargets(ctx context.Context, tx *writeTx, taskID int64, targets []ReviewTarget) error {
	for _, target := range targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_targets(task_id,repository,commit_sha) VALUES(?,?,?)`, taskID, target.Repository, target.Commit); err != nil {
			return err
		}
	}
	return nil
}

// ReviewTargets returns the commits a Review Task was bound to at admission.
func (db *DB) ReviewTargets(ctx context.Context, taskID int64) ([]ReviewTarget, error) {
	return queryReviewTargets(ctx, db, taskID)
}

type contextQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryReviewTargets(ctx context.Context, q contextQuerier, taskID int64) ([]ReviewTarget, error) {
	rows, err := q.QueryContext(ctx, `SELECT repository,commit_sha FROM review_targets WHERE task_id=? ORDER BY repository`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []ReviewTarget
	for rows.Next() {
		var target ReviewTarget
		if err := rows.Scan(&target.Repository, &target.Commit); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// ReviewVerdict returns the recorded verdict of a Review Task, or ErrNotFound.
func (db *DB) ReviewVerdict(ctx context.Context, taskID int64) (ReviewVerdict, error) {
	var verdict ReviewVerdict
	var targets string
	var modelKnown int
	err := db.QueryRowContext(ctx, `SELECT task_id,signal_id,verdict,report_sha256,targets_json,launch_number,profile_name,configured_model,model_known,created_at FROM review_verdicts WHERE task_id=?`, taskID).
		Scan(&verdict.TaskID, &verdict.SignalID, &verdict.Verdict, &verdict.ReportSHA256, &targets, &verdict.Reviewer.LaunchNumber, &verdict.Reviewer.Profile, &verdict.Reviewer.ConfiguredModel, &modelKnown, &verdict.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewVerdict{}, ErrNotFound
	}
	if err != nil {
		return ReviewVerdict{}, err
	}
	verdict.Reviewer.TaskID = taskID
	verdict.Reviewer.ModelKnown = modelKnown == 1
	if err := json.Unmarshal([]byte(targets), &verdict.Targets); err != nil {
		return ReviewVerdict{}, fmt.Errorf("decode review targets for Task %d: %w", taskID, err)
	}
	return verdict, nil
}

// ReviewVerdictsFor lists the verdicts of every Review Task of a Change Task,
// newest first.
func (db *DB) ReviewVerdictsFor(ctx context.Context, changeTaskID int64) ([]ReviewVerdict, error) {
	rows, err := db.QueryContext(ctx, `SELECT v.task_id FROM review_verdicts v JOIN tasks t ON t.id=v.task_id WHERE t.reviews_task_id=? ORDER BY v.created_at DESC, v.task_id DESC`, changeTaskID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	verdicts := make([]ReviewVerdict, 0, len(ids))
	for _, id := range ids {
		verdict, err := db.ReviewVerdict(ctx, id)
		if err != nil {
			return nil, err
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, nil
}

// CurrentReviewPass returns the newest pass verdict of the Change Task whose
// bound targets equal its current candidate commits. A candidate that has
// moved has no current pass, and an older pass is never rebound to the new
// commits because targets are fixed when the Review Task is admitted.
// qualifies lets the caller apply policy that Posse cannot see here, such as
// Reviewer identity eligibility.
func (db *DB) CurrentReviewPass(ctx context.Context, changeTaskID int64, candidate []ReviewTarget, qualifies func(ReviewVerdict) bool) (ReviewVerdict, bool, error) {
	verdicts, err := db.ReviewVerdictsFor(ctx, changeTaskID)
	if err != nil {
		return ReviewVerdict{}, false, err
	}
	for _, verdict := range verdicts {
		if verdict.Verdict != ReviewPass || !SameReviewTargets(verdict.Targets, candidate) {
			continue
		}
		if qualifies != nil && !qualifies(verdict) {
			continue
		}
		return verdict, true, nil
	}
	return ReviewVerdict{}, false, nil
}

// recordReviewVerdictTx writes the verdict inside the Signal transaction. The
// bound target, Reviewer launch identity and Review Task identity come from
// the store, never from the Worker.
func recordReviewVerdictTx(ctx context.Context, tx *writeTx, taskID, signalID int64, input ReviewVerdictInput) error {
	var taskType string
	var launches int
	var changeTaskID sql.NullInt64
	var projectID int64
	if err := tx.QueryRowContext(ctx, `SELECT type,launches,reviews_task_id,project_id FROM tasks WHERE id=?`, taskID).Scan(&taskType, &launches, &changeTaskID, &projectID); err != nil {
		return err
	}
	if taskType != "review" {
		return ErrReviewVerdictType
	}
	if !ValidReviewVerdict(input.Verdict) || len(input.ReportSHA256) != 64 {
		return fmt.Errorf("invalid review verdict %q", input.Verdict)
	}
	var roundCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_rounds WHERE review_task_id=?`, taskID).Scan(&roundCount); err != nil {
		return err
	}
	if roundCount > 0 {
		if !changeTaskID.Valid || changeTaskID.Int64 < 1 {
			return fmt.Errorf("%w: Review Round has no Change Task", ErrReviewFindingsInvalid)
		}
		if input.Findings == nil {
			return ErrReviewFindingsRequired
		}
		var changeTitle string
		if err := tx.QueryRowContext(ctx, `SELECT title FROM tasks WHERE id=?`, changeTaskID.Int64).Scan(&changeTitle); err != nil {
			return err
		}
		result, err := recordReviewFindingsTx(ctx, tx, changeTaskID.Int64, taskID, *input.Findings)
		if err != nil {
			return err
		}
		if input.Verdict != ReviewInconclusive && input.Verdict != result.DerivedVerdict {
			return fmt.Errorf("%w: Reviewer supplied %s, but the Finding Ledger derives %s", ErrReviewVerdictInconsistent, input.Verdict, result.DerivedVerdict)
		}
		updated, err := tx.ExecContext(ctx, `UPDATE review_rounds SET status='verdict',verdict=? WHERE review_task_id=? AND status='admitted'`, input.Verdict, taskID)
		if err != nil {
			return err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if changed != int64(roundCount) {
			return fmt.Errorf("%w: Review Round is no longer admitted", ErrReviewFindingsInvalid)
		}
		if err := insertReviewRoundNoticesTx(ctx, tx, projectID, changeTaskID.Int64, changeTitle, result, input.Verdict); err != nil {
			return err
		}
	} else if input.Findings != nil {
		return ErrReviewFindingsUnexpected
	}
	targets, err := queryReviewTargets(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return ErrReviewTargetUnbound
	}
	var reviewer TaskLaunchIdentity
	var modelKnown int
	err = tx.QueryRowContext(ctx, `SELECT launch_number,profile_name,configured_model,model_known FROM task_launch_identities WHERE task_id=? AND launch_number=?`, taskID, launches).
		Scan(&reviewer.LaunchNumber, &reviewer.Profile, &reviewer.ConfiguredModel, &modelKnown)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("review task has no launch identity for launch %d", launches)
	}
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_verdicts WHERE task_id=?`, taskID).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return ErrReviewVerdictFound
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_verdicts(task_id,signal_id,verdict,report_sha256,targets_json,launch_number,profile_name,configured_model,model_known,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		taskID, signalID, input.Verdict, input.ReportSHA256, string(encoded), reviewer.LaunchNumber, reviewer.Profile, reviewer.ConfiguredModel, modelKnown, time.Now().UnixMilli())
	return err
}
