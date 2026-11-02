package store

import (
	"context"
	"errors"
	"testing"
)

const (
	branchAuthorTestSHA  = "1111111111111111111111111111111111111111"
	branchAuthorOtherSHA = "2222222222222222222222222222222222222222"
)

func TestBranchAuthorClaimRequiresStoppedWriterAndExpectedHead(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Change", ShortName: "change"}, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", taskID, 1, branchAuthorTestSHA, branchAuthorTestSHA, false)
	if err != nil || first.Generation != 1 {
		t.Fatalf("initial claim = %#v, %v", first, err)
	}
	if _, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", taskID, 1, branchAuthorTestSHA, branchAuthorTestSHA, false); !errors.Is(err, ErrBranchAuthorWriterActive) {
		t.Fatalf("duplicate claim in the same attempt error = %v", err)
	}
	if _, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", taskID, 2, branchAuthorTestSHA, branchAuthorTestSHA, false); !errors.Is(err, ErrBranchAuthorWriterActive) {
		t.Fatalf("claim with an unstopped writer error = %v", err)
	}
	reviewID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "review", Title: "Review", ShortName: "review", ReviewsTaskID: taskID}, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET gated_sha=? WHERE id=?`, branchAuthorTestSHA, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO review_rounds(ship_task_id,round,review_task_id,repository,scope_kind,scope_base_sha,head_sha,target_commit_sha,baseline_tree,delta_sha256,conflicted_paths_json,status,verdict,created_at) VALUES(?,1,?,'shop','full',?,?,?,?,?,'[]','verdict','pass',1)`, taskID, reviewID, branchAuthorTestSHA, branchAuthorTestSHA, branchAuthorTestSHA, branchAuthorTestSHA, reportHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO approvals(task_id,action,user_quote,at) VALUES(?,'merge','approve',1),(?,'discard','approve',1)`, taskID, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,checks,review,mergeable,merge_commit,observed_at) VALUES(?,?, 'https://example.test/pr/1',?,'OPEN','SUCCESS','APPROVED','MERGEABLE','',1)`, project.ID, taskID, branchAuthorTestSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO merge_queue_entries(project_id,task_id,repo,target_branch,entered_at,created_at) VALUES(?,?,'','main',1,1)`, project.ID, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", taskID, 2, branchAuthorOtherSHA, branchAuthorTestSHA, true); !errors.Is(err, ErrBranchAuthorHeadChanged) {
		t.Fatalf("claim with a mismatched head error = %v", err)
	}
	second, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", taskID, 2, branchAuthorTestSHA, branchAuthorTestSHA, true)
	if err != nil || second.Generation != 2 {
		t.Fatalf("successor claim = %#v, %v", second, err)
	}
	if err := db.RequireBranchAuthor(ctx, "shop", "posse/t1", taskID, 1); !errors.Is(err, ErrBranchAuthorStale) {
		t.Fatalf("stale author error = %v", err)
	}
	if err := db.RequireBranchAuthor(ctx, "shop", "posse/t1", taskID, 2); err != nil {
		t.Fatalf("current author claim was refused: %v", err)
	}
	var gated, roundStatus string
	var approvalCount, observationCount, liveQueueCount int
	if err := db.QueryRowContext(ctx, `SELECT gated_sha FROM tasks WHERE id=?`, taskID).Scan(&gated); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM review_rounds WHERE ship_task_id=?`, taskID).Scan(&roundStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id=?`, taskID).Scan(&approvalCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pr_observations WHERE task_id=?`, taskID).Scan(&observationCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM merge_queue_entries WHERE task_id=? AND dequeued_at=0`, taskID).Scan(&liveQueueCount); err != nil {
		t.Fatal(err)
	}
	if gated != "" || roundStatus != "void" || approvalCount != 0 || observationCount != 0 || liveQueueCount != 0 {
		t.Fatalf("author transfer retained old evidence: gated=%q round=%q approvals=%d observations=%d queue=%d", gated, roundStatus, approvalCount, observationCount, liveQueueCount)
	}
	newTaskID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Maintenance", ShortName: "maintenance"}, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	third, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", newTaskID, 1, branchAuthorTestSHA, branchAuthorTestSHA, true)
	if err != nil || third.Generation != 3 {
		t.Fatalf("cross-Task claim = %#v, %v", third, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM branch_authorship`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild branch author claim: %v", err)
	}
	rebuilt, err := db.BranchAuthor(ctx, "shop", "posse/t1")
	if err != nil || rebuilt.TaskID != newTaskID || rebuilt.Attempt != 1 || rebuilt.Generation != 3 {
		t.Fatalf("rebuilt branch author claim = %#v, %v", rebuilt, err)
	}
}

func TestBranchAuthorClaimCanStartUnbornAndRecordFirstCommit(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Change", ShortName: "change"}, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	claim, err := db.ClaimBranchAuthor(ctx, "shop", "posse/t1", taskID, 1, "", "", false)
	if err != nil || claim.Generation != 1 {
		t.Fatalf("unborn branch claim = %#v, %v", claim, err)
	}
	if err := db.UpdateBranchAuthorHead(ctx, "shop", "posse/t1", taskID, claim.Generation, "", branchAuthorTestSHA); err != nil {
		t.Fatalf("record first commit: %v", err)
	}
	updated, err := db.BranchAuthor(ctx, "shop", "posse/t1")
	if err != nil || updated.HeadSHA != branchAuthorTestSHA {
		t.Fatalf("branch author after first commit = %#v, %v", updated, err)
	}
}

func TestBranchAuthorTransferInvalidatesReviewPass(t *testing.T) {
	targets := []ReviewTarget{{Repository: "shop", Commit: reviewShaA}}
	fixture := newReviewVerdictFixture(t, targets)
	if _, err := fixture.db.ClaimBranchAuthor(fixture.ctx, "/repo/.git", "posse/change", fixture.change, 1, reviewShaA, reviewShaA, false); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.RecordWorkerSignalWithVerdict(fixture.ctx, fixture.task(t, fixture.review), "done", "reviewed", map[string]string{}, "task_done", &ReviewVerdictInput{Verdict: ReviewPass, ReportSHA256: reportHash}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := fixture.db.CurrentReviewPass(fixture.ctx, fixture.change, targets); err != nil || !ok {
		var rounds, verdicts, joined int
		_ = fixture.db.QueryRowContext(fixture.ctx, `SELECT COUNT(*) FROM review_rounds WHERE ship_task_id=?`, fixture.change).Scan(&rounds)
		_ = fixture.db.QueryRowContext(fixture.ctx, `SELECT COUNT(*) FROM review_verdicts WHERE task_id=?`, fixture.review).Scan(&verdicts)
		_ = fixture.db.QueryRowContext(fixture.ctx, `SELECT COUNT(*) FROM review_verdicts v JOIN tasks t ON t.id=v.task_id JOIN review_rounds r ON r.review_task_id=v.task_id AND r.ship_task_id=t.reviews_task_id AND r.status='verdict' AND r.verdict=v.verdict WHERE t.reviews_task_id=?`, fixture.change).Scan(&joined)
		t.Fatalf("fresh Review pass = %t, %v; rounds=%d verdicts=%d joined=%d", ok, err, rounds, verdicts, joined)
	}
	if _, err := fixture.db.ClaimBranchAuthor(fixture.ctx, "/repo/.git", "posse/change", fixture.change, 2, reviewShaA, reviewShaA, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := fixture.db.CurrentReviewPass(fixture.ctx, fixture.change, targets); err != nil || ok {
		t.Fatalf("old Review pass remained current: pass=%t err=%v", ok, err)
	}
	if _, err := fixture.db.RebuildFromSnapshots(fixture.ctx, fixture.home); err != nil {
		t.Fatalf("rebuild invalidated Review verdict: %v", err)
	}
	if _, ok, err := fixture.db.CurrentReviewPass(fixture.ctx, fixture.change, targets); err != nil || ok {
		t.Fatalf("rebuilt old Review pass remained current: pass=%t err=%v", ok, err)
	}
}
