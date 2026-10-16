package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrIndependentReportRequired = errors.New("current independent Report-review pass is required before acceptance")

type ReportReview struct {
	ID             int64  `toml:"id" json:"id"`
	ReportTaskID   int64  `toml:"report_task_id" json:"report_task_id"`
	ReviewTaskID   int64  `toml:"review_task_id" json:"review_task_id"`
	ReportDigest   string `toml:"report_digest" json:"report_digest"`
	Verdict        string `toml:"verdict" json:"verdict"`
	ReviewerLaunch int    `toml:"reviewer_launch" json:"reviewer_launch"`
	RecordedAt     int64  `toml:"recorded_at" json:"recorded_at"`
}

type ReportAcceptance struct {
	ReportTaskID int64  `toml:"report_task_id" json:"report_task_id"`
	ReportDigest string `toml:"report_digest" json:"report_digest"`
	AcceptedBy   string `toml:"accepted_by" json:"accepted_by"`
	RecordedBy   string `toml:"recorded_by" json:"recorded_by"`
	Provenance   string `toml:"provenance" json:"provenance"`
	UserQuote    string `toml:"user_quote,omitempty" json:"user_quote,omitempty"`
	AcceptedAt   int64  `toml:"accepted_at" json:"accepted_at"`
}

func (db *DB) RecordReportBundle(ctx context.Context, taskID int64, digest, manifestJSON string) error {
	if manifestJSON == "" || digest != reportManifestDigest(manifestJSON) {
		return fmt.Errorf("report bundle digest must be the SHA-256 of its manifest")
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskType, state, currentDigest, currentManifest string
	if err := tx.QueryRowContext(ctx, `SELECT type,state,report_digest,report_manifest_json FROM tasks WHERE id=?`, taskID).Scan(&taskType, &state, &currentDigest, &currentManifest); err != nil {
		return err
	}
	if taskType != "scout" || State(state) != StateReported && State(state) != StateTornDown {
		return fmt.Errorf("report bundle can only be finalized for a reported Scout Task")
	}
	if currentDigest != "" {
		if currentDigest != digest || currentManifest != manifestJSON {
			return fmt.Errorf("report bundle is immutable; the finalized Report or attachments have changed")
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return db.PersistTask(ctx, taskID)
	}
	if State(state) != StateReported {
		return fmt.Errorf("cannot finalize a Report bundle for a Scout Task without a reported result")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET report_digest=?,report_manifest_json=?,updated_at=? WHERE id=? AND report_digest=''`, digest, manifestJSON, time.Now().UnixMilli(), taskID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) RecordReportReview(ctx context.Context, reportTaskID, reviewTaskID int64, digest, verdict string) (ReportReview, error) {
	if verdict != "pass" && verdict != "changes_requested" && verdict != "inconclusive" {
		return ReportReview{}, fmt.Errorf("invalid Report-review verdict %q", verdict)
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return ReportReview{}, err
	}
	defer tx.Rollback()
	var reportType, reportState, currentDigest, manifest string
	var reportProjectID int64
	var reportLaunches int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,type,state,report_digest,report_manifest_json,launches FROM tasks WHERE id=?`, reportTaskID).Scan(&reportProjectID, &reportType, &reportState, &currentDigest, &manifest, &reportLaunches); err != nil {
		return ReportReview{}, err
	}
	if reportType != "scout" || State(reportState) != StateTornDown || currentDigest == "" || manifest == "" || currentDigest != digest {
		return ReportReview{}, fmt.Errorf("report review must target the current finalized bundle of a torn-down Scout Task")
	}
	var reviewerType, reviewerState string
	var reviewsTaskID sql.NullInt64
	var reviewerLaunches int
	var reviewerProjectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id,type,state,reviews_task_id,launches FROM tasks WHERE id=?`, reviewTaskID).Scan(&reviewerProjectID, &reviewerType, &reviewerState, &reviewsTaskID, &reviewerLaunches); err != nil {
		return ReportReview{}, err
	}
	if reviewerProjectID != reportProjectID || reviewerType != "review" || !reviewsTaskID.Valid || reviewsTaskID.Int64 != reportTaskID || State(reviewerState) != StateReported && State(reviewerState) != StateTornDown {
		return ReportReview{}, fmt.Errorf("report review requires a completed Review Task linked to the Report Task")
	}
	identities, err := taskLaunchIdentitiesTx(ctx, tx.Tx, reportTaskID)
	if err != nil {
		return ReportReview{}, err
	}
	reviewerIdentities, err := taskLaunchIdentitiesTx(ctx, tx.Tx, reviewTaskID)
	if err != nil {
		return ReportReview{}, err
	}
	if reportLaunches == 0 || len(identities) != reportLaunches || !completeLaunchHistory(identities) || len(reviewerIdentities) != reviewerLaunches || !completeLaunchHistory(reviewerIdentities) {
		return ReportReview{}, fmt.Errorf("independent Report review requires complete author and reviewer launch identities")
	}
	for _, reviewer := range reviewerIdentities {
		if strings.TrimSpace(reviewer.Profile) == "" || !reviewer.ModelKnown || strings.TrimSpace(reviewer.ConfiguredModel) == "" {
			return ReportReview{}, fmt.Errorf("independent Report review requires a known reviewer Profile and configured model")
		}
		for _, author := range identities {
			if strings.TrimSpace(author.Profile) == "" || !author.ModelKnown || strings.TrimSpace(author.ConfiguredModel) == "" {
				return ReportReview{}, fmt.Errorf("independent Report review requires known identity for every author launch")
			}
			if reviewer.Profile == author.Profile || reviewer.ConfiguredModel == author.ConfiguredModel {
				return ReportReview{}, fmt.Errorf("reviewer launch %d is not independent of author launch %d", reviewer.LaunchNumber, author.LaunchNumber)
			}
		}
	}
	if reviewerLaunches == 0 || reviewerIdentities[len(reviewerIdentities)-1].LaunchNumber != reviewerLaunches {
		return ReportReview{}, fmt.Errorf("independent Report review has no current reviewer launch")
	}
	review := ReportReview{ReportTaskID: reportTaskID, ReviewTaskID: reviewTaskID, ReportDigest: digest, Verdict: verdict, ReviewerLaunch: reviewerLaunches, RecordedAt: time.Now().UnixMilli()}
	result, err := tx.ExecContext(ctx, `INSERT INTO report_reviews(report_task_id,review_task_id,report_digest,verdict,reviewer_launch,recorded_at) VALUES(?,?,?,?,?,?) ON CONFLICT(report_task_id,review_task_id,report_digest,reviewer_launch) DO NOTHING`, reportTaskID, reviewTaskID, digest, verdict, reviewerLaunches, review.RecordedAt)
	if err != nil {
		return ReportReview{}, err
	}
	if count, err := result.RowsAffected(); err != nil {
		return ReportReview{}, err
	} else if count == 0 {
		var existingVerdict string
		if err := tx.QueryRowContext(ctx, `SELECT verdict FROM report_reviews WHERE report_task_id=? AND review_task_id=? AND report_digest=? AND reviewer_launch=?`, reportTaskID, reviewTaskID, digest, reviewerLaunches).Scan(&existingVerdict); err != nil {
			return ReportReview{}, err
		}
		if existingVerdict != verdict {
			return ReportReview{}, fmt.Errorf("report-review verdict for this reviewer launch is immutable")
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT id,recorded_at FROM report_reviews WHERE report_task_id=? AND review_task_id=? AND report_digest=? AND reviewer_launch=?`, reportTaskID, reviewTaskID, digest, reviewerLaunches).Scan(&review.ID, &review.RecordedAt); err != nil {
		return ReportReview{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReportReview{}, err
	}
	if err := db.PersistTask(ctx, reportTaskID); err != nil {
		return ReportReview{}, err
	}
	return review, nil
}

func (db *DB) ReportReviewsForTask(ctx context.Context, reportTaskID int64) ([]ReportReview, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,report_task_id,review_task_id,report_digest,verdict,reviewer_launch,recorded_at FROM report_reviews WHERE report_task_id=? ORDER BY recorded_at,id`, reportTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reviews []ReportReview
	for rows.Next() {
		var review ReportReview
		if err := rows.Scan(&review.ID, &review.ReportTaskID, &review.ReviewTaskID, &review.ReportDigest, &review.Verdict, &review.ReviewerLaunch, &review.RecordedAt); err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, rows.Err()
}

func (db *DB) ReportAcceptanceForTask(ctx context.Context, reportTaskID int64) (*ReportAcceptance, error) {
	var acceptance ReportAcceptance
	err := db.QueryRowContext(ctx, `SELECT report_task_id,report_digest,accepted_by,recorded_by,provenance,user_quote,accepted_at FROM report_acceptances WHERE report_task_id=?`, reportTaskID).Scan(
		&acceptance.ReportTaskID, &acceptance.ReportDigest, &acceptance.AcceptedBy, &acceptance.RecordedBy, &acceptance.Provenance, &acceptance.UserQuote, &acceptance.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &acceptance, nil
}

func (db *DB) AcceptReport(ctx context.Context, reportTaskID int64, digest, recordedBy, userQuote string) (ReportAcceptance, error) {
	if recordedBy != "user" && recordedBy != "lead" {
		return ReportAcceptance{}, fmt.Errorf("report acceptance must be recorded by the User or Lead")
	}
	provenance := "user-command"
	if recordedBy == "lead" {
		if strings.TrimSpace(userQuote) == "" {
			return ReportAcceptance{}, fmt.Errorf("explicit User approval quote is required")
		}
		provenance = "lead-quote"
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return ReportAcceptance{}, err
	}
	defer tx.Rollback()
	var taskType, taskState, reportKind, currentDigest, manifest string
	var reportProjectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id,type,state,report_kind,report_digest,report_manifest_json FROM tasks WHERE id=?`, reportTaskID).Scan(&reportProjectID, &taskType, &taskState, &reportKind, &currentDigest, &manifest); err != nil {
		return ReportAcceptance{}, err
	}
	if taskType != "scout" || reportKind != "spec" && reportKind != "tickets" {
		return ReportAcceptance{}, fmt.Errorf("only Scout Reports marked report_kind spec or tickets require acceptance")
	}
	if State(taskState) != StateTornDown || currentDigest == "" || currentDigest != digest || manifest == "" || reportManifestDigest(manifest) != currentDigest {
		return ReportAcceptance{}, fmt.Errorf("acceptance requires the current finalized Report bundle of a torn-down Scout Task")
	}
	var existing ReportAcceptance
	err = tx.QueryRowContext(ctx, `SELECT report_task_id,report_digest,accepted_by,recorded_by,provenance,user_quote,accepted_at FROM report_acceptances WHERE report_task_id=?`, reportTaskID).Scan(
		&existing.ReportTaskID, &existing.ReportDigest, &existing.AcceptedBy, &existing.RecordedBy, &existing.Provenance, &existing.UserQuote, &existing.AcceptedAt)
	if err == nil {
		if existing.ReportDigest != digest {
			return ReportAcceptance{}, fmt.Errorf("an acceptance already exists for a different Report digest")
		}
		if err := tx.Commit(); err != nil {
			return ReportAcceptance{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ReportAcceptance{}, err
	}
	if err := requireIndependentReportPass(ctx, tx.Tx, reportTaskID, reportProjectID, digest); err != nil {
		return ReportAcceptance{}, err
	}
	acceptance := ReportAcceptance{ReportTaskID: reportTaskID, ReportDigest: digest, AcceptedBy: "user", RecordedBy: recordedBy, Provenance: provenance, UserQuote: userQuote, AcceptedAt: time.Now().UnixMilli()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO report_acceptances(report_task_id,report_digest,accepted_by,recorded_by,provenance,user_quote,accepted_at) VALUES(?,?,?,?,?,?,?)`, acceptance.ReportTaskID, acceptance.ReportDigest, acceptance.AcceptedBy, acceptance.RecordedBy, acceptance.Provenance, acceptance.UserQuote, acceptance.AcceptedAt); err != nil {
		return ReportAcceptance{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReportAcceptance{}, err
	}
	if err := db.PersistTask(ctx, reportTaskID); err != nil {
		return ReportAcceptance{}, err
	}
	return acceptance, nil
}

func requireIndependentReportPass(ctx context.Context, tx *sql.Tx, reportTaskID, reportProjectID int64, digest string) error {
	rows, err := tx.QueryContext(ctx, `SELECT review_task_id,reviewer_launch,verdict FROM report_reviews WHERE report_task_id=? AND report_digest=? ORDER BY id DESC`, reportTaskID, digest)
	if err != nil {
		return err
	}
	type candidate struct {
		taskID  int64
		launch  int
		verdict string
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.taskID, &item.launch, &item.verdict); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var authorLaunches int
	if err := tx.QueryRowContext(ctx, `SELECT launches FROM tasks WHERE id=?`, reportTaskID).Scan(&authorLaunches); err != nil {
		return err
	}
	authors, err := taskLaunchIdentitiesTx(ctx, tx, reportTaskID)
	if err != nil {
		return err
	}
	if authorLaunches == 0 || len(authors) != authorLaunches || !completeLaunchHistory(authors) {
		return ErrIndependentReportRequired
	}
	for _, candidate := range candidates {
		var taskType, state string
		var linkedTask sql.NullInt64
		var launches int
		var reviewerProjectID int64
		if err := tx.QueryRowContext(ctx, `SELECT project_id,type,state,reviews_task_id,launches FROM tasks WHERE id=?`, candidate.taskID).Scan(&reviewerProjectID, &taskType, &state, &linkedTask, &launches); err != nil {
			continue
		}
		if reviewerProjectID != reportProjectID || taskType != "review" || !linkedTask.Valid || linkedTask.Int64 != reportTaskID || State(state) != StateReported && State(state) != StateTornDown || candidate.launch != launches {
			continue
		}
		reviewers, err := taskLaunchIdentitiesTx(ctx, tx, candidate.taskID)
		if err != nil || len(reviewers) != launches || !completeLaunchHistory(reviewers) {
			continue
		}
		eligible := true
		for _, reviewer := range reviewers {
			if reviewer.Profile == "" || !reviewer.ModelKnown || strings.TrimSpace(reviewer.ConfiguredModel) == "" {
				eligible = false
				break
			}
			for _, author := range authors {
				if author.Profile == "" || !author.ModelKnown || strings.TrimSpace(author.ConfiguredModel) == "" || reviewer.Profile == author.Profile || reviewer.ConfiguredModel == author.ConfiguredModel {
					eligible = false
					break
				}
			}
		}
		if eligible {
			if candidate.verdict == "pass" {
				return nil
			}
			return ErrIndependentReportRequired
		}
	}
	return ErrIndependentReportRequired
}

func taskLaunchIdentitiesTx(ctx context.Context, tx *sql.Tx, taskID int64) ([]TaskLaunchIdentity, error) {
	rows, err := tx.QueryContext(ctx, `SELECT task_id,launch_number,profile_name,configured_model,model_known,plan_revision FROM task_launch_identities WHERE task_id=? ORDER BY launch_number`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var identities []TaskLaunchIdentity
	for rows.Next() {
		var identity TaskLaunchIdentity
		if err := rows.Scan(&identity.TaskID, &identity.LaunchNumber, &identity.Profile, &identity.ConfiguredModel, &identity.ModelKnown, &identity.PlanRevision); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

func reportManifestDigest(manifest string) string {
	digest := sha256.Sum256([]byte(manifest))
	return hex.EncodeToString(digest[:])
}

func completeLaunchHistory(identities []TaskLaunchIdentity) bool {
	if len(identities) == 0 {
		return false
	}
	for index, identity := range identities {
		if identity.LaunchNumber != index+1 || identity.TaskID == 0 || strings.TrimSpace(identity.Profile) == "" {
			return false
		}
	}
	return true
}
