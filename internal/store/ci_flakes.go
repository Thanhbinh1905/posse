package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CITestFailure is one Go test a failed CI log tail named on a PR head.
type CITestFailure struct {
	ProjectID  int64
	TaskID     int64
	Repo       string
	PRURL      string
	HeadSHA    string
	CheckName  string
	JobID      string
	Package    string
	Test       string
	ObservedAt int64
}

// CIRerun is one rerun of a failed CI check. Reason is cancelled, flake or
// steward; Outcome is pending until a completed observation of the same head
// shows whether the check failed again.
type CIRerun struct {
	ID          int64
	ProjectID   int64
	TaskID      int64
	Repo        string
	PRURL       string
	HeadSHA     string
	CheckName   string
	RunID       string
	JobID       string
	Reason      string
	Tests       []string
	Outcome     string
	RequestedAt int64
	SettledAt   int64
}

// RecordCITestFailures keeps each failure once per head, check job and test.
func (db *DB) RecordCITestFailures(ctx context.Context, failures []CITestFailure) error {
	for _, failure := range failures {
		if failure.ProjectID < 1 || strings.TrimSpace(failure.PRURL) == "" || strings.TrimSpace(failure.Test) == "" {
			return fmt.Errorf("CI test failure requires a Project, PR and test")
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO ci_test_failures(project_id,task_id,repo,pr_url,head_sha,check_name,job_id,package,test,observed_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
			failure.ProjectID, failure.TaskID, failure.Repo, failure.PRURL, failure.HeadSHA, failure.CheckName, failure.JobID, failure.Package, failure.Test, failure.ObservedAt); err != nil {
			return err
		}
	}
	return nil
}

// CITestFailureRecorded reports whether a job's failures were already kept for
// head under prURL.
func (db *DB) CITestFailureRecorded(ctx context.Context, projectID int64, prURL, headSHA, checkName, jobID string) (bool, error) {
	var found bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ci_test_failures WHERE project_id=? AND pr_url=? AND head_sha=? AND check_name=? AND job_id=?)`, projectID, prURL, headSHA, checkName, jobID).Scan(&found)
	return found, err
}

// CITestFailedElsewhere reports whether test in pkg failed on a PR other
// than prURL at or after since.
func (db *DB) CITestFailedElsewhere(ctx context.Context, projectID int64, prURL, pkg, test string, since int64) (bool, error) {
	var found bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ci_test_failures WHERE project_id=? AND test=? AND package=? AND pr_url<>? AND observed_at>=?)`, projectID, test, pkg, prURL, since).Scan(&found)
	return found, err
}

// RecordCIRerun stores a rerun request as pending. A second flake rerun of the
// same check on the same head is refused with inserted false.
func (db *DB) RecordCIRerun(ctx context.Context, rerun CIRerun) (id int64, inserted bool, err error) {
	if rerun.ProjectID < 1 || rerun.TaskID < 1 || strings.TrimSpace(rerun.PRURL) == "" || !validCommitSHA(rerun.HeadSHA) || strings.TrimSpace(rerun.CheckName) == "" {
		return 0, false, fmt.Errorf("invalid CI rerun record")
	}
	if rerun.Reason != "cancelled" && rerun.Reason != "flake" && rerun.Reason != "steward" {
		return 0, false, fmt.Errorf("invalid CI rerun reason %q", rerun.Reason)
	}
	tests := rerun.Tests
	if tests == nil {
		tests = []string{}
	}
	encoded, err := json.Marshal(tests)
	if err != nil {
		return 0, false, err
	}
	if rerun.RequestedAt == 0 {
		rerun.RequestedAt = time.Now().UnixMilli()
	}
	result, err := db.ExecContext(ctx, `INSERT INTO ci_reruns(project_id,task_id,repo,pr_url,head_sha,check_name,run_id,job_id,reason,tests_json,requested_at) VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		rerun.ProjectID, rerun.TaskID, rerun.Repo, rerun.PRURL, rerun.HeadSHA, rerun.CheckName, rerun.RunID, rerun.JobID, rerun.Reason, string(encoded), rerun.RequestedAt)
	if err != nil {
		return 0, false, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return 0, false, err
	}
	id, err = result.LastInsertId()
	return id, err == nil, err
}

// FailCIRerunRequest marks a rerun the forge refused.
func (db *DB) FailCIRerunRequest(ctx context.Context, id int64) error {
	_, err := db.ExecContext(ctx, `UPDATE ci_reruns SET outcome='request_failed', settled_at=? WHERE id=? AND outcome='pending'`, time.Now().UnixMilli(), id)
	return err
}

// SettleCIReruns resolves the pending reruns on a completed head: a check
// still in the set failing returns failed again, any other passed. failing is
// called only when a rerun is pending.
func (db *DB) SettleCIReruns(ctx context.Context, taskID int64, repo, prURL, headSHA string, failing func() (map[string]bool, error)) error {
	rows, err := db.QueryContext(ctx, `SELECT id,check_name FROM ci_reruns WHERE task_id=? AND repo=? AND pr_url=? AND head_sha=? AND outcome='pending'`, taskID, repo, prURL, headSHA)
	if err != nil {
		return err
	}
	pending := map[int64]string{}
	for rows.Next() {
		var id int64
		var check string
		if err := rows.Scan(&id, &check); err != nil {
			rows.Close()
			return err
		}
		pending[id] = check
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil || len(pending) == 0 {
		return err
	}
	failed, err := failing()
	if err != nil {
		return err
	}
	outcomes := make(map[int64]string, len(pending))
	for id, check := range pending {
		outcomes[id] = "passed"
		if failed[check] {
			outcomes[id] = "failed"
		}
	}
	now := time.Now().UnixMilli()
	for id, outcome := range outcomes {
		if _, err := db.ExecContext(ctx, `UPDATE ci_reruns SET outcome=?, settled_at=? WHERE id=? AND outcome='pending'`, outcome, now, id); err != nil {
			return err
		}
	}
	return nil
}

// CIReruns lists the newest reruns of a Task's PR, newest first.
func (db *DB) CIReruns(ctx context.Context, taskID int64, repo, prURL string, limit int) ([]CIRerun, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,task_id,repo,pr_url,head_sha,check_name,run_id,job_id,reason,tests_json,outcome,requested_at,settled_at FROM ci_reruns WHERE task_id=? AND repo=? AND pr_url=? ORDER BY id DESC LIMIT ?`, taskID, repo, prURL, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reruns := []CIRerun{}
	for rows.Next() {
		var rerun CIRerun
		var tests string
		if err := rows.Scan(&rerun.ID, &rerun.ProjectID, &rerun.TaskID, &rerun.Repo, &rerun.PRURL, &rerun.HeadSHA, &rerun.CheckName, &rerun.RunID, &rerun.JobID, &rerun.Reason, &tests, &rerun.Outcome, &rerun.RequestedAt, &rerun.SettledAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tests), &rerun.Tests); err != nil {
			return nil, err
		}
		reruns = append(reruns, rerun)
	}
	return reruns, rows.Err()
}
