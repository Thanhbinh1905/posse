package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func greenhouseVerdict(taskID int64, grant, head string) Verdict {
	return Verdict{TaskID: taskID, Repository: "shop", Provider: GreenhouseReviewProvider, Protocol: "2", Grant: grant,
		Head: head, Target: reviewShaA, ReviewBase: reviewShaA, Outcome: "clean", Mode: "parallel"}
}

func TestGreenhouseReviewRequestLinksGateModeVerdict(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	verdict := greenhouseVerdict(f.change, "shop/t1/shop/r1", reviewShaA)
	verdict.Mode = "gate"
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, verdict, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record gate Verdict = %v, %v", recorded, err)
	}
	request := GreenhouseReviewRequest{ProjectID: f.task(t, f.change).ProjectID, TaskID: f.change, Repository: "shop", RequestedHead: reviewShaA,
		Provider: GreenhouseReviewProvider, RequestedAt: 10, SettledAt: 20, Outcome: "verdict", Grant: verdict.Grant}
	if err := f.db.RecordGreenhouseReviewRequest(f.ctx, request); err != nil {
		t.Fatalf("record request for gate Verdict: %v", err)
	}
	requests, err := f.db.GreenhouseReviewRequests(f.ctx, request.ProjectID)
	if err != nil || len(requests) != 1 || requests[0].VerdictID == nil {
		t.Fatalf("gate Verdict request link = %+v, %v", requests, err)
	}
}

func TestRecordGreenhouseVerdictChecksHeadInsideTheInsert(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	headNow := func(head string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return head, nil }
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouseVerdict(f.change, "shop/t1/shop/r1", reviewShaB), headNow(reviewShaA)); !errors.Is(err, ErrVerdictHeadMoved) || recorded {
		t.Fatalf("a moved head recorded = %v, err = %v; want ErrVerdictHeadMoved", recorded, err)
	}
	if verdicts, err := f.db.VerdictsFor(f.ctx, f.change); err != nil || len(verdicts) != 0 {
		t.Fatalf("Verdicts after a refused insert = %+v, %v", verdicts, err)
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouseVerdict(f.change, "shop/t1/shop/r2", reviewShaB), headNow(reviewShaB)); err != nil || !recorded {
		t.Fatalf("an unmoved head recorded = %v, err = %v", recorded, err)
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouseVerdict(f.change, "shop/t1/shop/r2", reviewShaB), headNow(reviewShaB)); err != nil || recorded {
		t.Fatalf("a second Verdict for grant r2 recorded = %v, err = %v", recorded, err)
	}
	verdicts, err := f.db.VerdictsFor(f.ctx, f.change)
	if err != nil || len(verdicts) != 1 || verdicts[0].Provider != GreenhouseReviewProvider || verdicts[0].Grant != "shop/t1/shop/r2" || verdicts[0].Qualifies(reviewShaB) {
		t.Fatalf("Verdicts = %+v, %v; want one parallel Greenhouse Verdict that no gate accepts", verdicts, err)
	}
}

func TestGreenhouseAndBuiltinRoundsShareReviewCapHistory(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','verdict','changes_requested',1)`,
		f.change, f.review, reviewShaA, reviewShaA, reviewShaA, reviewShaA, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	greenhouse := greenhouseVerdict(f.change, "shop/t1/shop/r4", reviewShaB)
	greenhouse.Mode = "gate"
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouse, func(context.Context) (string, error) { return reviewShaB, nil }); err != nil || !recorded {
		t.Fatalf("record Greenhouse round = %v, %v", recorded, err)
	}
	usage, err := f.db.ReviewCapUsage(f.ctx, f.change)
	if err != nil || usage.ReviewRounds != 2 {
		t.Fatalf("Review cap usage = %+v, %v; want earlier built-in round plus Greenhouse round", usage, err)
	}
}

func TestGreenhouseVerdictPersistsFindingsByTaskRepositoryAndHead(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	verdict := greenhouseVerdict(f.change, "shop/t1/shop/r3", reviewShaA)
	verdict.Outcome, verdict.Mode = "findings", "gate"
	verdict.Findings = []ProviderFinding{{Key: "G-1", Severity: "P1", Origin: "correctness", Path: "internal/app/tasks.go", Line: 42,
		Excerpt: "The task can publish without review", Guidance: "Require the Verdict", Rule: "review-gate", Head: reviewShaA}}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, verdict, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record Verdict with findings = %v, %v", recorded, err)
	}
	findings, err := f.db.ProviderFindingsForTask(f.ctx, f.change)
	if err != nil || len(findings) != 1 || findings[0].Repository != "shop" || findings[0].HeadSHA != reviewShaA || findings[0].State != "open" || findings[0].Rule != "review-gate" {
		t.Fatalf("Greenhouse findings = %+v, %v", findings, err)
	}
}

func TestProviderFindingsUseReviewFixQueueAndResolution(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	verdict := greenhouseVerdict(f.change, "shop/t1/shop/r5", reviewShaA)
	verdict.Outcome, verdict.Mode = "findings", "gate"
	verdict.Blocking = 1
	verdict.Findings = []ProviderFinding{{Key: "G-1", Severity: "P1", Origin: "correctness", Path: "internal/app/tasks.go", Line: 42,
		Excerpt: "The gate is skipped", Guidance: "Require the gate", Rule: "gate-bypass", Head: reviewShaA}}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, verdict, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record findings Verdict = %v, %v", recorded, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','verdict','pass',1)`,
		f.change, f.review, reviewShaA, reviewShaA, reviewShaA, reviewShaA, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	maxRounds := 1
	queued, err := f.db.QueueProviderReviewFixInstruction(f.ctx, f.change, reviewShaA, nil, "review:"+reviewShaA+":P-1", "Fix P-1", []int{1}, &maxRounds, &maxRounds)
	if err != nil || !queued.Queued {
		t.Fatalf("queue Greenhouse Review fix = %+v, %v", queued, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE messages SET status='delivered',delivered_at=created_at WHERE id=?`, queued.MessageID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.beginTxWithRetry(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := &ReviewFixResolutionsInput{ExpectedBaseHead: reviewShaA, ActualHead: reviewShaB, Patterns: []ReviewFixResolution{{Pattern: "P-1", Status: "fixed", Note: "Required the gate"}}}
	if err := applyReviewFixResolutionsTx(f.ctx, tx, f.change, input); err != nil {
		tx.Rollback()
		t.Fatalf("apply Greenhouse Review fix resolution: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	findings, err := f.db.ProviderFindingsForTask(f.ctx, f.change)
	if err != nil || len(findings) != 1 || findings[0].State != "fix_claimed" {
		t.Fatalf("Greenhouse finding after resolution = %+v, %v", findings, err)
	}
}

func TestGreenhouseGatePassRequiresLatestCurrentCleanVerdict(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	pass := greenhouseVerdict(f.change, "shop/t1/shop/r1", reviewShaA)
	pass.Mode = "gate"
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, pass, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record passing gate Verdict = %v, %v", recorded, err)
	}
	if ok, err := f.db.GreenhouseGatePass(f.ctx, f.change, "shop", reviewShaA); err != nil || !ok {
		t.Fatalf("current passing gate Verdict = %v, %v", ok, err)
	}
	inconclusive := greenhouseVerdict(f.change, "shop/t1/shop/r2", reviewShaB)
	inconclusive.Mode, inconclusive.Outcome = "gate", "inconclusive"
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, inconclusive, func(context.Context) (string, error) { return reviewShaB, nil }); err != nil || !recorded {
		t.Fatalf("record inconclusive gate Verdict = %v, %v", recorded, err)
	}
	if ok, err := f.db.GreenhouseGatePass(f.ctx, f.change, "shop", reviewShaB); err != nil || ok {
		t.Fatalf("inconclusive Verdict passed = %v, %v", ok, err)
	}
	if ok, err := f.db.GreenhouseGatePass(f.ctx, f.change, "shop", reviewShaA); err != nil || ok {
		t.Fatalf("stale earlier pass passed = %v, %v", ok, err)
	}
}

func TestGreenhouseReviewRequestLedgerIsImmutableAndRecordsEveryOutcome(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	requests := []GreenhouseReviewRequest{
		{ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: reviewShaA, Provider: GreenhouseReviewProvider, RequestedAt: 100, SettledAt: 120, Outcome: "failed"},
		{ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: reviewShaA, Provider: GreenhouseReviewProvider, RequestedAt: 130, SettledAt: 150, Outcome: "head_mismatch"},
	}
	for _, request := range requests {
		if err := f.db.RecordGreenhouseReviewRequest(f.ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.db.GreenhouseReviewRequests(f.ctx, task.ProjectID)
	if err != nil || len(got) != 2 || got[0].Outcome != "failed" || got[1].Outcome != "head_mismatch" {
		t.Fatalf("request ledger = %+v, %v", got, err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE greenhouse_review_requests SET outcome='verdict' WHERE id=?`, got[0].ID); err == nil {
		t.Fatal("updated an insert-only request row")
	}
}

func TestGreenhouseReviewLedgerKeepsResumeOutcomeForSameGrant(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	grant := "shop/t1/shop/r1"
	failed := GreenhouseReviewRequest{ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: reviewShaA, Provider: GreenhouseReviewProvider, RequestedAt: 100, SettledAt: 200, Outcome: "failed", Grant: grant}
	if err := f.db.RecordGreenhouseReviewRequest(f.ctx, failed); err != nil {
		t.Fatal(err)
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, greenhouseVerdict(task.ID, grant, reviewShaA), func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record resumed Verdict = %v, %v", recorded, err)
	}
	settled := failed
	settled.SettledAt, settled.Outcome = 310, "verdict"
	if err := f.db.RecordGreenhouseReviewRequest(f.ctx, settled); err != nil {
		t.Fatal(err)
	}
	requests, err := f.db.GreenhouseReviewRequests(f.ctx, task.ProjectID)
	if err != nil || len(requests) != 1 || requests[0].Outcome != "verdict" || requests[0].VerdictID == nil || requests[0].RequestedAt != failed.RequestedAt {
		t.Fatalf("resumed request rows = %+v, %v; want one request with its final outcome", requests, err)
	}
}

func TestGreenhouseRequestMigrationConsolidatesResumedGrantHistory(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	grant := "shop/t1/shop/r1"
	verdict := greenhouseVerdict(task.ID, grant, reviewShaA)
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, verdict, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record resumed Verdict = %v, %v", recorded, err)
	}
	var verdictID int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT id FROM verdicts WHERE grant_id=?`, grant).Scan(&verdictID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX greenhouse_review_requests_grant_idx`,
		`DROP TRIGGER greenhouse_review_requests_immutable`,
		`CREATE TRIGGER greenhouse_review_requests_immutable BEFORE UPDATE ON greenhouse_review_requests BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is immutable'); END`,
	} {
		if _, err := f.db.ExecContext(f.ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, request := range []struct {
		requestedAt, settledAt int64
		outcome                string
		verdictID              any
	}{{100, 200, "failed", nil}, {100, 300, "verdict", verdictID}} {
		if _, err := f.db.ExecContext(f.ctx, `INSERT INTO greenhouse_review_requests(project_id,task_id,repo,requested_head,provider,requested_at,settled_at,outcome,verdict_id,grant_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			task.ProjectID, task.ID, "shop", reviewShaA, GreenhouseReviewProvider, request.requestedAt, request.settledAt, request.outcome, request.verdictID, grant); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.ExecContext(f.ctx, `DELETE FROM posse_migration_checksums WHERE version=98`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `DELETE FROM goose_db_version WHERE version_id=98`); err != nil {
		t.Fatal(err)
	}
	path := f.db.Path
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAt(path)
	if err != nil {
		t.Fatalf("apply request-ledger migration to resumed history: %v", err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM greenhouse_review_requests WHERE task_id=? AND repo=? AND grant_id=?`, task.ID, "shop", grant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows for resumed grant = %d, %v; want one", count, err)
	}
	var requestedAt, settledAt int64
	var outcome string
	var gotVerdictID int64
	if err := reopened.QueryRowContext(f.ctx, `SELECT requested_at,settled_at,outcome,verdict_id FROM greenhouse_review_requests WHERE task_id=? AND repo=? AND grant_id=?`, task.ID, "shop", grant).Scan(&requestedAt, &settledAt, &outcome, &gotVerdictID); err != nil {
		t.Fatal(err)
	}
	if requestedAt != 100 || settledAt != 300 || outcome != "verdict" || gotVerdictID != verdictID {
		t.Fatalf("migrated request = requested %d, settled %d, outcome %q, Verdict %d", requestedAt, settledAt, outcome, gotVerdictID)
	}
}

func TestGreenhouseFailureNoticeOncePerHead(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	for _, reason := range []string{"network failure", "protocol refusal"} {
		if _, err := f.db.CreateGreenhouseFailureNoticeOnce(f.ctx, task.ProjectID, task.ID, "shop", reviewShaA, reason); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='greenhouse_review_failed'`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failure Notices = %d, %v; want one per head", count, err)
	}
	var summary string
	if err := f.db.QueryRowContext(f.ctx, `SELECT summary FROM notices WHERE task_id=? AND kind='greenhouse_review_failed'`, task.ID).Scan(&summary); err != nil || !strings.Contains(summary, "protocol refusal") {
		t.Fatalf("latest failure summary = %q, %v; want latest reason", summary, err)
	}
}

func TestGreenhouseRecoveryNoticeOncePerRecovery(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	for _, recovery := range []string{"rec-1", "rec-1", "rec-2"} {
		if _, err := f.db.CreateGreenhouseRecoveryNoticeOnce(f.ctx, task.ProjectID, task.ID, "shop", recovery, "parked"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='greenhouse_recovery_required'`, task.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("recovery Notices = %d, %v; want one per recovery", count, err)
	}
}
