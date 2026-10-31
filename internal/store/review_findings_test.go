package store

import (
	"encoding/json"
	"errors"
	"testing"
)

const reviewShaC = "dddddddddddddddddddddddddddddddddddddddd"

func addFindingsReviewRound(t *testing.T, f reviewVerdictFixture, sequence, round int, head string) int64 {
	t.Helper()
	ctx := f.ctx
	change := f.task(t, f.change)
	reviewID := f.review
	if sequence != 2 || round != 1 {
		var err error
		reviewID, err = f.db.CreateTask(ctx, change.ProjectID, Task{
			Seq: sequence, Type: "review", ReviewsTaskID: f.change, Title: "Review round",
			MergeMode: "local", ReviewTargets: []ReviewTarget{{Repository: "shop", Commit: head}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.NextTaskLaunchWithIdentity(ctx, reviewID, "reviewer", "model-b", true); err != nil {
			t.Fatal(err)
		}
		if err := f.db.Transition(ctx, reviewID, StateSpawning, StateWorking, "cli", "started"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,?,?,'shop','full',?,?,?,?,?,'[]','admitted',NULL,1)`,
		f.change, round, reviewID, head, head, head, head, reportHash); err != nil {
		t.Fatal(err)
	}
	return reviewID
}

func findingsInput(key, severity string, inScope bool) *ReviewFindingsInput {
	return &ReviewFindingsInput{
		BlockAt: "P1",
		Patterns: []ReviewFindingPatternInput{{
			Key: key, Title: "Dropped errors", Rule: "Check all writes", RootCause: "Errors are ignored", FixGuidance: "Check every new call",
			Severity: severity, Origin: "introduced",
			Instances: []ReviewFindingInput{{Repository: "shop", Path: "internal/app/x.go", Symbol: "save", Excerpt: "db.Exec(ctx, q)", Line: 12, InScope: inScope}},
		}},
	}
}

func TestReviewFindingsFailClosedAndDeriveVerdictAtomically(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	firstReview := addFindingsReviewRound(t, f, 2, 1, reviewShaA)
	missingSeverity := findingsInput("Check---Write Errors", "unclassified", true)
	pass := &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash, Findings: missingSeverity}
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, firstReview), "done", "reviewed", map[string]string{}, "task_done", pass); !errors.Is(err, ErrReviewVerdictInconsistent) {
		t.Fatalf("pass with an unclassified in-scope finding error = %v", err)
	}
	if f.task(t, firstReview).State != StateWorking || f.signalCount(t, firstReview) != 0 {
		t.Fatal("refused pass partially recorded a Signal")
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=?`, f.change).Scan(&count); err != nil || count != 0 {
		t.Fatalf("refused pass inserted %d findings: %v", count, err)
	}
	changes := *pass
	changes.Verdict = ReviewChangesRequested
	if state, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, firstReview), "done", "reviewed", map[string]string{}, "task_done", &changes); err != nil || state != StateReported {
		t.Fatalf("changes-requested state=%s err=%v", state, err)
	}
	var key, severity, origin, state string
	var inScope int
	if err := f.db.QueryRowContext(f.ctx, `SELECT p.key,f.severity,f.origin,f.state,f.in_scope FROM review_findings f JOIN review_patterns p ON p.id=f.pattern_id WHERE f.ship_task_id=?`, f.change).
		Scan(&key, &severity, &origin, &state, &inScope); err != nil {
		t.Fatal(err)
	}
	if key != "check-write-errors" || severity != "unclassified" || origin != "introduced" || state != "open" || inScope != 1 {
		t.Fatalf("finding projection = %s %s %s %s in_scope=%d", key, severity, origin, state, inScope)
	}
	verdict, err := f.db.ReviewVerdict(f.ctx, firstReview)
	if err != nil || verdict.Verdict != ReviewChangesRequested {
		t.Fatalf("derived verdict = %#v, %v", verdict, err)
	}
}

func TestReviewFindingsSuppressRepeatsAndRegressOnTheSameFinding(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	firstReview := addFindingsReviewRound(t, f, 2, 1, reviewShaA)
	first := findingsInput("Dropped---Errors", "P1", true)
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, firstReview), "done", "findings", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewChangesRequested, ReportSHA256: reportHash, Findings: first}); err != nil {
		t.Fatal(err)
	}
	var findingID int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT id FROM review_findings WHERE ship_task_id=?`, f.change).Scan(&findingID); err != nil {
		t.Fatal(err)
	}

	secondReview := addFindingsReviewRound(t, f, 3, 2, reviewShaB)
	repeat := findingsInput(" dropped errors ", "P1", true)
	repeat.Patterns[0].Instances[0].Excerpt = " db.Exec(ctx,\n q) "
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, secondReview), "done", "repeat", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewChangesRequested, ReportSHA256: reportHash, Findings: repeat}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM review_findings WHERE ship_task_id=?`, f.change).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeat created %d findings: %v", count, err)
	}
	var repeatData string
	if err := f.db.QueryRowContext(f.ctx, `SELECT data_json FROM notices WHERE task_id=? AND kind='review_round_done' ORDER BY id DESC LIMIT 1`, f.change).Scan(&repeatData); err != nil {
		t.Fatal(err)
	}
	var roundNotice map[string]any
	if err := json.Unmarshal([]byte(repeatData), &roundNotice); err != nil || roundNotice["suppressed_repeats"] != float64(1) {
		t.Fatalf("repeat count notice = %s (%v)", repeatData, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_findings SET state='fixed' WHERE id=?`, findingID); err != nil {
		t.Fatal(err)
	}

	thirdReview := addFindingsReviewRound(t, f, 4, 3, reviewShaC)
	regression := findingsInput("dropped errors", "P1", true)
	regression.Patterns[0].Instances[0].Regresses = "F-1"
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, thirdReview), "done", "regressed", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewChangesRequested, ReportSHA256: reportHash, Findings: regression}); err != nil {
		t.Fatal(err)
	}
	var afterID int64
	var state string
	if err := f.db.QueryRowContext(f.ctx, `SELECT id,state FROM review_findings WHERE ship_task_id=? AND seq=1`, f.change).Scan(&afterID, &state); err != nil || afterID != findingID || state != "regressed" {
		t.Fatalf("regression created a new ID or lost state: id=%d state=%s err=%v", afterID, state, err)
	}
	var event string
	if err := f.db.QueryRowContext(f.ctx, `SELECT event FROM review_finding_events WHERE finding_id=? ORDER BY id DESC LIMIT 1`, findingID).Scan(&event); err != nil || event != "regressed" {
		t.Fatalf("latest finding event = %q, %v", event, err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='review_regression'`, f.change).Scan(&count); err != nil || count != 1 {
		t.Fatalf("regression notices = %d, %v", count, err)
	}
	if _, err := f.db.RebuildFromSnapshots(f.ctx, f.home); err != nil {
		t.Fatalf("rebuild snapshots: %v", err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT id,state FROM review_findings WHERE ship_task_id=? AND seq=1`, f.change).Scan(&afterID, &state); err != nil || afterID != findingID || state != "regressed" {
		t.Fatalf("rebuilt regression = id %d state %s err=%v", afterID, state, err)
	}
}

func TestReviewFindingsResolutionsMarkFixedOrUnverified(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	firstReview := addFindingsReviewRound(t, f, 2, 1, reviewShaA)
	initial := findingsInput("unchecked-write", "P1", true)
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, firstReview), "done", "finding", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewChangesRequested, ReportSHA256: reportHash, Findings: initial}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_findings SET state='fix_claimed' WHERE ship_task_id=? AND seq=1`, f.change); err != nil {
		t.Fatal(err)
	}
	secondReview := addFindingsReviewRound(t, f, 3, 2, reviewShaB)
	resolved := &ReviewFindingsInput{BlockAt: "P1", Patterns: []ReviewFindingPatternInput{}, Resolutions: []ReviewFindingResolutionInput{{Finding: "F-1", Status: "resolved", Note: "verified at head"}}}
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, secondReview), "done", "resolved", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash, Findings: resolved}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.db.QueryRowContext(f.ctx, `SELECT state FROM review_findings WHERE ship_task_id=? AND seq=1`, f.change).Scan(&state); err != nil || state != "fixed" {
		t.Fatalf("resolved finding state=%s err=%v", state, err)
	}

	thirdReview := addFindingsReviewRound(t, f, 4, 3, reviewShaC)
	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_findings SET state='fix_claimed' WHERE ship_task_id=? AND seq=1`, f.change); err != nil {
		t.Fatal(err)
	}
	unverified := &ReviewFindingsInput{BlockAt: "P1", Patterns: []ReviewFindingPatternInput{}}
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, thirdReview), "done", "missing resolution", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewChangesRequested, ReportSHA256: reportHash, Findings: unverified}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT state FROM review_findings WHERE ship_task_id=? AND seq=1`, f.change).Scan(&state); err != nil || state != "unverified" {
		t.Fatalf("missing resolution state=%s err=%v", state, err)
	}
}

func TestReviewFindingsResolutionsCloseUnclaimedAndIgnoreDisposed(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	firstReview := addFindingsReviewRound(t, f, 2, 1, reviewShaA)
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, firstReview), "done", "finding", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewChangesRequested, ReportSHA256: reportHash, Findings: findingsInput("unchecked-write", "P1", true)}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ApplyReviewDisposition(f.ctx, f.change, nil, []int{1}, "withdraw", "", "", "not a defect", "quote", "user", "P1"); err != nil {
		t.Fatal(err)
	}
	secondReview := addFindingsReviewRound(t, f, 3, 2, reviewShaB)
	stale := &ReviewFindingsInput{BlockAt: "P1", Patterns: []ReviewFindingPatternInput{}, Resolutions: []ReviewFindingResolutionInput{{Finding: "F-1", Status: "unresolved"}}}
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, secondReview), "done", "pass", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash, Findings: stale}); err != nil {
		t.Fatalf("pass with a resolution for a withdrawn finding: %v", err)
	}
	var state string
	if err := f.db.QueryRowContext(f.ctx, `SELECT state FROM review_findings WHERE ship_task_id=? AND seq=1`, f.change).Scan(&state); err != nil || state != "withdrawn" {
		t.Fatalf("withdrawn finding state=%s err=%v", state, err)
	}

	if _, err := f.db.ExecContext(f.ctx, `UPDATE review_findings SET state='open' WHERE ship_task_id=? AND seq=1`, f.change); err != nil {
		t.Fatal(err)
	}
	thirdReview := addFindingsReviewRound(t, f, 4, 3, reviewShaC)
	verified := &ReviewFindingsInput{BlockAt: "P1", Patterns: []ReviewFindingPatternInput{}, Resolutions: []ReviewFindingResolutionInput{{Finding: "F-1", Status: "resolved", Note: "fixed without a claim"}}}
	if _, err := f.db.RecordWorkerSignalWithVerdict(f.ctx, f.task(t, thirdReview), "done", "pass", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash, Findings: verified}); err != nil {
		t.Fatalf("resolving an open finding without a fix claim: %v", err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT state FROM review_findings WHERE ship_task_id=? AND seq=1`, f.change).Scan(&state); err != nil || state != "fixed" {
		t.Fatalf("open finding verified fixed state=%s err=%v", state, err)
	}
}
