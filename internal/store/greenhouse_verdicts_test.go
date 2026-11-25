package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func greenhouseVerdict(taskID int64, grant, head string) Verdict {
	return Verdict{TaskID: taskID, Repository: "shop", Provider: GreenhouseReviewProvider, Protocol: "2", Grant: grant,
		Head: head, Target: reviewShaA, ReviewBase: reviewShaA, Outcome: "clean", Mode: "parallel"}
}

func TestGreenhouseFailureCountMigrationBackfillsGrantlessFailures(t *testing.T) {
	f := newReviewVerdictFixture(t, nil)
	projectID := f.task(t, f.change).ProjectID
	request := GreenhouseReviewRequest{
		ProjectID: projectID, TaskID: f.change, Repository: "shop", RequestedHead: reviewShaA,
		Provider: GreenhouseReviewProvider, RequestedAt: 10, SettledAt: 20, Outcome: "failed",
	}
	if err := f.db.RecordGreenhouseReviewRequest(f.ctx, request); err != nil {
		t.Fatal(err)
	}
	grantRequest := request
	grantRequest.RequestedAt, grantRequest.Grant = 11, "shop/t1/shop/r1"
	if err := f.db.RecordGreenhouseReviewRequest(f.ctx, grantRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `DROP TRIGGER greenhouse_review_requests_immutable`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `CREATE TRIGGER greenhouse_review_requests_immutable BEFORE UPDATE ON greenhouse_review_requests
WHEN NOT (
    OLD.outcome = 'failed' AND OLD.grant_id IS NOT NULL AND
    NEW.id = OLD.id AND NEW.project_id = OLD.project_id AND NEW.task_id = OLD.task_id AND
    NEW.repo = OLD.repo AND NEW.requested_head = OLD.requested_head AND NEW.provider = OLD.provider AND
    NEW.requested_at = OLD.requested_at AND NEW.grant_id = OLD.grant_id AND
    NEW.settled_at >= OLD.settled_at AND NEW.outcome IN ('failed', 'verdict', 'refused', 'head_mismatch')
)
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is immutable'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `ALTER TABLE greenhouse_review_requests DROP COLUMN failure_count`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `DELETE FROM goose_db_version WHERE version_id=20261123132235`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `DELETE FROM posse_migration_checksums WHERE version=20261123132235`); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(f.home)
	if err != nil {
		t.Fatalf("open home with a grant-less failed request: %v", err)
	}
	defer db.Close()
	requests, err := db.GreenhouseReviewRequests(f.ctx, projectID)
	if err != nil || len(requests) != 2 || requests[0].FailureCount != 1 || requests[1].FailureCount != 3 {
		t.Fatalf("backfilled requests = %+v, %v; want one grant-less failure and a limit-count granted failure", requests, err)
	}
	if _, err := db.ExecContext(f.ctx, `UPDATE greenhouse_review_requests SET outcome='head_mismatch' WHERE id=?`, requests[0].ID); err == nil {
		t.Fatal("updated a grant-less request after migration")
	}
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

func TestWithdrawingLastP1ProviderFindingQualifiesExactHead(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	verdict := greenhouseVerdict(f.change, "shop/t1/shop/r-withdraw", reviewShaA)
	verdict.Outcome, verdict.Mode, verdict.Blocking = "findings", "gate", 1
	verdict.Findings = []ProviderFinding{{Key: "G-1", Severity: "P1", Origin: "correctness", Path: "internal/app/tasks.go", Line: 42, Excerpt: "False finding", Head: reviewShaA}}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, verdict, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record P1 Verdict = %v, %v", recorded, err)
	}
	findings, err := f.db.ProviderFindingsForTask(f.ctx, f.change)
	if err != nil || len(findings) != 1 {
		t.Fatalf("provider findings = %+v, %v", findings, err)
	}
	if err := f.db.WithdrawProviderFinding(f.ctx, f.change, int64(findings[0].FindingSeq), 17, strings.Repeat("a", 64), "User-approved dispute"); err != nil {
		t.Fatal(err)
	}
	if passed, err := f.db.GreenhouseGatePass(f.ctx, f.change, "shop", reviewShaA); err != nil || !passed {
		t.Fatalf("withdrawn exact-head finding did not qualify: passed=%v err=%v", passed, err)
	}
	findings, err = f.db.ProviderFindingsForTask(f.ctx, f.change)
	if err != nil || len(findings) != 1 || findings[0].State != "withdrawn" {
		t.Fatalf("withdrawn finding projection = %+v, %v", findings, err)
	}
}

func TestWithdrawnProviderFindingCannotEnterReviewFixRound(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	verdict := greenhouseVerdict(f.change, "shop/t1/shop/r-withdraw-fix", reviewShaA)
	verdict.Outcome, verdict.Mode, verdict.Blocking = "findings", "gate", 1
	verdict.Findings = []ProviderFinding{{Key: "G-1", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "withdrawn", Head: reviewShaA}}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, verdict, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record P1 Verdict = %v, %v", recorded, err)
	}
	findings, err := f.db.ProviderFindingsForTask(f.ctx, f.change)
	if err != nil || len(findings) != 1 {
		t.Fatalf("provider findings = %+v, %v", findings, err)
	}
	if err := f.db.WithdrawProviderFinding(f.ctx, f.change, int64(findings[0].FindingSeq), 17, strings.Repeat("a", 64), "User-approved dispute"); err != nil {
		t.Fatal(err)
	}
	_, err = f.db.QueueProviderReviewFixInstruction(f.ctx, f.change, reviewShaA, nil, "review:"+reviewShaA+":P-1", "Fix P-1", []int{1}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no current findings") {
		t.Fatalf("queue fix for withdrawn finding = %v; want no current findings", err)
	}
	var bindings int
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM fix_round_provider_findings WHERE finding_id=?`, findings[0].FindingSeq).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("withdrawn finding fix-round bindings = %d, %v; want none", bindings, err)
	}
}

func TestWithdrawnProviderFindingCannotBeMutatedByPendingFix(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	verdict := greenhouseVerdict(f.change, "shop/t1/shop/r-withdraw-pending", reviewShaA)
	verdict.Outcome, verdict.Mode, verdict.Blocking = "findings", "gate", 1
	verdict.Findings = []ProviderFinding{{Key: "G-1", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "withdrawn", Head: reviewShaA}}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, verdict, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record P1 Verdict = %v, %v", recorded, err)
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
	findings, err := f.db.ProviderFindingsForTask(f.ctx, f.change)
	if err != nil || len(findings) != 1 {
		t.Fatalf("provider findings = %+v, %v", findings, err)
	}
	if err := f.db.WithdrawProviderFinding(f.ctx, f.change, int64(findings[0].FindingSeq), 17, strings.Repeat("a", 64), "User-approved dispute"); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.beginTxWithRetry(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	input := &ReviewFixResolutionsInput{ExpectedBaseHead: reviewShaA, ActualHead: reviewShaB, Patterns: []ReviewFixResolution{{Pattern: "P-1", Status: "fixed"}}}
	if err := applyReviewFixResolutionsTx(f.ctx, tx, f.change, input); err == nil {
		tx.Rollback()
		t.Fatal("pending fix mutated a withdrawn finding")
	}
	tx.Rollback()
	var state string
	if err := f.db.QueryRowContext(f.ctx, `SELECT state FROM provider_findings WHERE task_id=?`, f.change).Scan(&state); err != nil || state != "open" {
		t.Fatalf("underlying finding state = %q, %v; want unchanged open state after rejected stale resolution", state, err)
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

func TestGreenhouseGatePassReleasesVerdictRowsBeforeFindingQuery(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	verdict := greenhouseVerdict(f.change, "shop/t1/shop/r-single-connection", reviewShaA)
	verdict.Outcome, verdict.Mode, verdict.Blocking = "findings", "gate", 1
	verdict.Findings = []ProviderFinding{{Key: "G-1", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "false finding", Head: reviewShaA}}
	if recorded, err := f.db.RecordGreenhouseVerdict(f.ctx, verdict, func(context.Context) (string, error) { return reviewShaA, nil }); err != nil || !recorded {
		t.Fatalf("record P1 Verdict = %v, %v", recorded, err)
	}
	findings, err := f.db.ProviderFindingsForTask(f.ctx, f.change)
	if err != nil || len(findings) != 1 {
		t.Fatalf("provider findings = %+v, %v", findings, err)
	}
	if err := f.db.WithdrawProviderFinding(f.ctx, f.change, int64(findings[0].FindingSeq), 17, strings.Repeat("a", 64), "User-approved dispute"); err != nil {
		t.Fatal(err)
	}
	f.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	passed, err := f.db.GreenhouseGatePass(ctx, f.change, "shop", reviewShaA)
	if err != nil || !passed {
		t.Fatalf("single-connection withdrawn finding gate = passed %v, err %v; want pass without waiting for a second connection", passed, err)
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
	for _, update := range []string{
		`UPDATE greenhouse_review_requests SET settled_at=201,outcome='failed',failure_count=99 WHERE task_id=? AND grant_id=?`,
		`UPDATE greenhouse_review_requests SET settled_at=201,outcome='verdict',failure_count=0 WHERE task_id=? AND grant_id=?`,
	} {
		if _, err := f.db.ExecContext(f.ctx, update, task.ID, grant); err == nil {
			t.Fatalf("trigger accepted an invalid failure-count update: %s", update)
		}
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
	var noticeID int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT id FROM notices WHERE task_id=? AND kind='greenhouse_review_failed'`, task.ID).Scan(&noticeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `UPDATE notices SET delivered_at=10,acked_at=11,claim_token='delivery-owner',claimed_at=12 WHERE id=?`, noticeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO notice_delivery_receipts(delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,accepted_at,created_at,updated_at) VALUES('delivery-1','batch-1',?,'[' || ? || ']','test','generation','claimed','delivery-owner',12,20,0,10,12)`, task.ProjectID, noticeID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := f.db.CreateGreenhouseNeedsDecisionNoticeOnce(f.ctx, task.ProjectID, task.ID, "shop", reviewShaA, "Needs a decision: latest context overflow"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='needs_decision'`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("same-head needs-decision Notices = %d, %v; want one", count, err)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind IN ('greenhouse_review_failed','needs_decision')`, task.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("failure and needs-decision Notices = %d, %v; want one of each", count, err)
	}
	var kind, claim string
	var delivered, acked sql.NullInt64
	var claimedAt int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT kind,summary,delivered_at,acked_at,claim_token,claimed_at FROM notices WHERE task_id=? AND kind='needs_decision'`, task.ID).Scan(&kind, &summary, &delivered, &acked, &claim, &claimedAt); err != nil || kind != "needs_decision" || !strings.Contains(summary, "context overflow") || delivered.Valid || acked.Valid || claim != "" || claimedAt != 0 {
		t.Fatalf("needs-decision Notice = kind %q summary %q delivered %v acked %v claim %q claimed_at %d err=%v", kind, summary, delivered, acked, claim, claimedAt, err)
	}
	var oldSummary string
	var oldAcked sql.NullInt64
	var oldClaim string
	if err := f.db.QueryRowContext(f.ctx, `SELECT summary,acked_at,claim_token FROM notices WHERE id=?`, noticeID).Scan(&oldSummary, &oldAcked, &oldClaim); err != nil || !strings.Contains(oldSummary, "protocol refusal") || !oldAcked.Valid || oldClaim != "" {
		t.Fatalf("old failure Notice summary=%q acked=%v claim=%q err=%v", oldSummary, oldAcked, oldClaim, err)
	}
	var receiptState, receiptOwner string
	if err := f.db.QueryRowContext(f.ctx, `SELECT state,owner_token FROM notice_delivery_receipts WHERE delivery_id='delivery-1'`).Scan(&receiptState, &receiptOwner); err != nil || receiptState != "rejected" || receiptOwner != "" {
		t.Fatalf("old Notice delivery receipt = state %q owner %q err=%v", receiptState, receiptOwner, err)
	}
	var needsDecisionID int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT id FROM notices WHERE task_id=? AND kind='needs_decision'`, task.ID).Scan(&needsDecisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.AckNotices(f.ctx, task.ProjectID, []string{strconv.FormatInt(needsDecisionID, 10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateGreenhouseNeedsDecisionNoticeOnce(f.ctx, task.ProjectID, task.ID, "shop", reviewShaA, "Needs a decision: latest context overflow"); err != nil {
		t.Fatal(err)
	}
	var needsDecisionAcked sql.NullInt64
	if err := f.db.QueryRowContext(f.ctx, `SELECT acked_at FROM notices WHERE id=?`, needsDecisionID).Scan(&needsDecisionAcked); err != nil || !needsDecisionAcked.Valid {
		t.Fatalf("repeated needs-decision event cleared acknowledgement: acked=%v err=%v", needsDecisionAcked, err)
	}
}

func TestGreenhouseNeedsDecisionPreservesUncertainFailureDelivery(t *testing.T) {
	f := newReviewVerdictFixture(t, []ReviewTarget{{Repository: "shop", Commit: reviewShaA}})
	task := f.task(t, f.change)
	if _, err := f.db.CreateGreenhouseFailureNoticeOnce(f.ctx, task.ProjectID, task.ID, "shop", reviewShaA, "review setup failed: missing Greenhouse binary"); err != nil {
		t.Fatal(err)
	}
	var noticeID int64
	if err := f.db.QueryRowContext(f.ctx, `SELECT id FROM notices WHERE task_id=? AND kind='greenhouse_review_failed'`, task.ID).Scan(&noticeID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := f.db.ExecContext(f.ctx, `UPDATE notices SET claim_token='delivery-owner',claimed_at=? WHERE id=?`, now, noticeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(f.ctx, `INSERT INTO notice_delivery_receipts(delivery_id,batch_id,project_id,notice_ids_json,destination,generation,state,owner_token,claimed_at,lease_until,accepted_at,created_at,updated_at) VALUES('delivery-uncertain','batch-uncertain',?,'[' || ? || ']','test','generation','printed','delivery-owner',?,?,0,?,?)`, task.ProjectID, noticeID, now, now+1000, now, now); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkNoticeDeliveryUncertain(f.ctx, "delivery-uncertain", now+1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateGreenhouseNeedsDecisionNoticeOnce(f.ctx, task.ProjectID, task.ID, "shop", reviewShaA, "Needs a decision: missing Greenhouse binary"); err != nil {
		t.Fatal(err)
	}
	var oldKind, oldClaim string
	var oldAcked sql.NullInt64
	if err := f.db.QueryRowContext(f.ctx, `SELECT kind,acked_at,claim_token FROM notices WHERE id=?`, noticeID).Scan(&oldKind, &oldAcked, &oldClaim); err != nil || oldKind != "greenhouse_review_failed" || !oldAcked.Valid || oldClaim != "uncertain:delivery-uncertain" {
		t.Fatalf("old Notice kind=%q acked=%v claim=%q err=%v; want acknowledged failure with its uncertain claim intact", oldKind, oldAcked, oldClaim, err)
	}
	if err := f.db.ResolveUncertainNoticeDelivery(f.ctx, task.ProjectID, "delivery-uncertain", "accepted", now+2); err != nil {
		t.Fatalf("resolve old uncertain delivery after escalation: %v", err)
	}
	var state string
	var delivered sql.NullInt64
	if err := f.db.QueryRowContext(f.ctx, `SELECT kind,delivered_at FROM notices WHERE task_id=? AND kind='needs_decision'`, task.ID).Scan(&state, &delivered); err != nil || delivered.Valid {
		t.Fatalf("escalated Notice kind=%q delivered=%v err=%v; want a new open needs_decision Notice", state, delivered, err)
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
