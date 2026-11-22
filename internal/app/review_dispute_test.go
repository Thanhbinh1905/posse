package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReviewDisputeDecisionWithdrawsOnlyAtBoundHead(t *testing.T) {
	for _, moveHead := range []bool{false, true} {
		t.Run(map[bool]string{false: "withdraw and queue publish", true: "refuse changed head"}[moveHead], func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateDone)
			defer f.db.Close()
			ctx := context.Background()
			verdict := store.Verdict{TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider, Protocol: "2", Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA, ReviewBase: f.headSHA, Outcome: "findings", Blocking: 1, Mode: "gate", Findings: []store.ProviderFinding{{Key: "G-1", Severity: "P1", Origin: "correctness", Path: "main.go", Line: 1, Excerpt: "false claim", Head: f.headSHA}}}
			if recorded, err := f.db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
				t.Fatalf("record blocking Verdict: %v, %v", recorded, err)
			}
			cap, err := f.db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "review_cap", Origin: reviewCapOrigin(f.task, f.headSHA), Question: "Review cap reached?", Options: []string{"accept-head", "rethink"}})
			if err != nil {
				t.Fatal(err)
			}
			evidence := filepath.Join(t.TempDir(), "evidence.md")
			if err := os.WriteFile(evidence, []byte("Test: validation at main.go:1 proves the reported behavior cannot occur.\nHead: "+f.headSHA+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if code, output, _ := f.run("review", "dispute", "t1", "F-1", "--evidence", evidence); code != 0 {
				t.Fatalf("create dispute Decision: %d %s", code, output)
			}
			var decisionID int64
			if err := f.db.QueryRowContext(ctx, `SELECT id FROM decisions WHERE task_id=? AND kind='review_dispute'`, f.task.ID).Scan(&decisionID); err != nil {
				t.Fatal(err)
			}
			decision, err := f.db.Decision(ctx, f.project.ID, decisionID)
			if err != nil || !strings.Contains(string(decision.Evidence), "Test: validation at main.go:1") || decision.EvidenceDigest == "" {
				t.Fatalf("stored evidence = %+v, %v", decision, err)
			}
			if code, output, _ := f.run("decide", strconv.FormatInt(decisionID, 10), "withdraw", "--user-approved", "I reviewed the evidence and withdraw this finding."); code != 0 {
				t.Fatalf("answer dispute Decision: %d %s", code, output)
			}
			if moveHead {
				if err := os.WriteFile(filepath.Join(f.worktree, "moved.txt"), []byte("head moved\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				gitTest(t, f.worktree, "add", "moved.txt")
				gitTest(t, f.worktree, "commit", "-m", "move Task head")
				if code, output, _ := f.run("apply", strconv.FormatInt(decisionID, 10)); code == 0 || !strings.Contains(output, "head changed") {
					t.Fatalf("apply at changed head = %d %s", code, output)
				}
				return
			}
			if code, output, _ := f.run("apply", strconv.FormatInt(decisionID, 10)); code != 0 {
				t.Fatalf("apply withdraw: %d %s", code, output)
			}
			passed, err := f.db.GreenhouseGatePass(ctx, f.task.ID, f.project.Name, f.headSHA)
			if err != nil || !passed {
				t.Fatalf("withdrawn finding did not permit exact-head publication: pass=%v err=%v", passed, err)
			}
			var queued int
			if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND body LIKE '%Publish this exact head%' AND body LIKE '%Fixes #1169%' AND body LIKE '%Fixes #1135%'`, f.task.ID).Scan(&queued); err != nil || queued != 1 {
				t.Fatalf("publish instruction count = %d, %v", queued, err)
			}
			var obsolete int
			if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions WHERE id=? AND obsolete_at>0 AND obsolete_reason LIKE '%' || ? || '%'`, cap.ID, strconv.FormatInt(decisionID, 10)).Scan(&obsolete); err != nil || obsolete != 1 {
				t.Fatalf("pending cap supersession count = %d, %v", obsolete, err)
			}
		})
	}
}
