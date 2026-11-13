package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReviewComparisonUsesAttemptRowsAndReportsG1PrecisionGap(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	ctx := context.Background()
	head, err := gitOutput(ctx, f.task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	findingsPath := filepath.Join(t.TempDir(), "findings.json")
	if err := os.WriteFile(findingsPath, []byte(`[{"file":"main.go","explanation":"The caller drops the returned error."}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO review_patterns(ship_task_id,seq,key,title,rule,root_cause,fix_guidance,created_round) VALUES(?,1,'dropped-error','Dropped error','Check writes','The caller drops the returned error','Check the caller',1)`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	var patternID int64
	if err := f.db.QueryRowContext(ctx, `SELECT id FROM review_patterns WHERE ship_task_id=?`, f.task.ID).Scan(&patternID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO review_findings(ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state) VALUES(?,1,?,? ,1,?,'main.go','save','return err',1,?,'P1','introduced',1,'fixed')`, f.task.ID, patternID, strings.Repeat("a", 64), f.project.Name, head); err != nil {
		t.Fatal(err)
	}
	failedHead := strings.Repeat("b", 40)
	if _, err := f.db.ExecContext(ctx, `INSERT INTO review_findings(ship_task_id,seq,pattern_id,fingerprint,round,repository,path,symbol,excerpt,line_at_report,head_sha,severity,origin,in_scope,state) VALUES(?,2,?,? ,1,?,'main.go','save','return err',1,?,'P1','introduced',1,'fixed')`, f.task.ID, patternID, strings.Repeat("b", 64), f.project.Name, failedHead); err != nil {
		t.Fatal(err)
	}
	grant := "shop/t1/shop/r1"
	verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: grant, Head: head, Target: head, ReviewBase: head, Outcome: "inconclusive", FindingsPath: findingsPath, Mode: "parallel"}
	if recorded, err := f.db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return head, nil }); err != nil || !recorded {
		t.Fatalf("record inconclusive parallel Verdict: recorded=%v err=%v", recorded, err)
	}
	base := store.GreenhouseReviewRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Repository: f.project.Name, RequestedHead: head, Provider: store.GreenhouseReviewProvider}
	for i, outcome := range []string{"verdict", "failed", "head_mismatch", "failed", "verdict"} {
		request := base
		if i == 3 {
			request.RequestedHead = failedHead
		}
		request.RequestedAt = int64(100 + i*100)
		request.SettledAt = request.RequestedAt + 50
		request.Outcome = outcome
		if outcome == "verdict" || i == 1 {
			request.Grant = grant
		}
		if err := f.db.RecordGreenhouseReviewRequest(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	row, _, err := reviewComparisonForProject(ctx, f.db, f.project)
	if err != nil {
		t.Fatal(err)
	}
	if row.ReviewedHeads != 1 || row.Requests != 5 || row.Verdicts != 2 || row.Reliability != 0.4 || row.WrongHead != 1 || row.WrongHeadRate != 0.2 || row.Inconclusive != 2 || row.InconclusiveShare != 1 || row.Precision != "not measured in G1" || row.Recall == nil || *row.Recall != 0.5 || row.RecallMatched != 1 || row.RecallPairs != 2 || len(row.UnmatchedPairs) != 1 || !strings.Contains(row.UnmatchedPairs[0], "builtin-only") {
		t.Fatalf("review comparison = %+v", row)
	}
	if row.MedianLatencyMS == nil || *row.MedianLatencyMS != 50 {
		t.Fatalf("median latency = %v, want 50ms", row.MedianLatencyMS)
	}
}
