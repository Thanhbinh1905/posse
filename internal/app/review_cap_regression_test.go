package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestFullPipelineReviewFixRejectsNonBlockingPatterns(t *testing.T) {
	plan := &taskplan.Plan{Review: "full-pipeline", ValidationLoops: &taskplan.ValidationLoops{Review: config.ReviewLoop{BlockAt: "P1"}}}
	findings := []store.ReviewFindingDetail{{PatternSeq: 2, Severity: "P3", State: "open", Origin: "introduced", InScope: true}}
	if selectedReviewPatternsBlock(findings, map[int]bool{2: true}, plan) {
		t.Fatal("non-blocking pattern qualified as a blocking-finding repair")
	}
	findings = append(findings, store.ReviewFindingDetail{PatternSeq: 3, Severity: "P1", State: "open", Origin: "introduced", InScope: true})
	if !selectedReviewPatternsBlock(findings, map[int]bool{2: true, 3: true}, plan) {
		t.Fatal("selected blocking pattern did not qualify the repair round")
	}
}

func TestGreenhouseNonBlockingFixNeedsFinalFixApproval(t *testing.T) {
	ctx := context.Background()
	f := newPRMergeFixture(t, "pr", store.StateDone)
	freezeCILoopWithReview(t, f, 0, 0, "full-pipeline")
	plan, err := f.task.AdmittedPlan()
	if err != nil {
		t.Fatal(err)
	}
	plan.ReviewProvider = store.GreenhouseReviewProvider
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET plan_json=? WHERE id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE task_plan_revisions SET plan_json=? WHERE task_id=?`, encoded, f.task.ID); err != nil {
		t.Fatal(err)
	}
	verdict := store.Verdict{
		TaskID: f.task.ID, Repository: f.project.Name, Provider: store.GreenhouseReviewProvider,
		Protocol: "2", Grant: "shop/t1/shop/r1", Head: f.headSHA, Target: f.headSHA,
		ReviewBase: f.headSHA, Outcome: "findings", Mode: "gate",
		Findings: []store.ProviderFinding{{Key: "non-blocking", Severity: "P3", Origin: "correctness", Path: "change.txt", Line: 1, Excerpt: "worker change", Guidance: "Clarify the wording", Head: f.headSHA}},
	}
	if recorded, err := f.db.RecordGreenhouseVerdict(ctx, verdict, func(context.Context) (string, error) { return f.headSHA, nil }); err != nil || !recorded {
		t.Fatalf("record non-blocking Verdict: recorded=%v err=%v", recorded, err)
	}
	if code, out, errOut := f.run("review", "fix", "t1", "--pattern", "P-1"); code != 1 || !strings.Contains(out+errOut, "review_fix_non_blocking") {
		t.Fatalf("ordinary Greenhouse fix spent a blocking repair round on a P3: %d %s%s", code, out, errOut)
	}
	if usage, err := f.db.ReviewCapUsage(ctx, f.task.ID); err != nil || usage.RepairRounds != 0 {
		t.Fatalf("non-blocking finding spent repair capacity: %+v, %v", usage, err)
	}
	seedHistoricalValidationFixRounds(t, f, 3)
	if _, err := f.db.ExecContext(ctx, `UPDATE messages SET created_at=0 WHERE task_id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := f.run("review", "fix", "t1"); code != 1 || !strings.Contains(out+errOut, "3 of 3 blocking-finding Review fix rounds used") {
		t.Fatalf("expected the spent cap to count only repairs: %d %s%s", code, out, errOut)
	}
	if code, out, errOut := f.run("decisions"); code != 0 || !strings.Contains(out, "final-fix") {
		t.Fatalf("expected a final-fix option at the cap: %d %s%s", code, out, errOut)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "review_cap" {
		t.Fatalf("expected one Review-cap Decision: %+v, %v", decisions, err)
	}
	id := fmt.Sprint(decisions[0].ID)
	if code, out, errOut := f.run("decide", id, "final-fix", "--user-approved", "Approve a final repair of the remaining finding."); code != 0 {
		t.Fatalf("answer final-fix: %d %s%s", code, out, errOut)
	}
	if code, out, errOut := f.run("apply", id); code != 0 || !strings.Contains(out, "final-fix-queued") || !strings.Contains(out, "P-1") {
		t.Fatalf("approved non-blocking fix was not queued: %d %s%s", code, out, errOut)
	}
	grant, err := f.db.ReviewCapGrant(ctx, f.task.ID)
	if err != nil || grant.DecisionID != decisions[0].ID || grant.Round != 2 {
		t.Fatalf("expected one final Review allowance after the recorded Verdict: %+v, %v", grant, err)
	}
}

func TestFullPipelineReviewCapCountsOnlyBlockingFindingRepairs(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	installValidationPlanForReview(t, fixture, "full-pipeline")
	task, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := []string{"findings", "clean", "clean", "findings"}
	for i, outcome := range outcomes {
		head := strings.Repeat("0", 39) + string(rune('1'+i))
		verdict := store.Verdict{TaskID: task.ID, Repository: "shop", Provider: store.GreenhouseReviewProvider, Protocol: "2",
			Grant: "shop/t1/review-" + string(rune('1'+i)), Head: head, Target: head, ReviewBase: head,
			Outcome: outcome, Mode: "gate"}
		if outcome == "findings" {
			verdict.Blocking = 1
		}
		if recorded, err := fixture.db.RecordGreenhouseVerdict(context.Background(), verdict, func(context.Context) (string, error) { return head, nil }); err != nil || !recorded {
			t.Fatalf("record review %d (%s): recorded=%v err=%v", i+1, outcome, recorded, err)
		}
	}
	if _, err := fixture.db.QueueValidationFixInstruction(context.Background(), task.ID, "review", strings.Repeat("0", 39)+"1", "review:first-findings", "Fix blocking findings"); err != nil {
		t.Fatal(err)
	}
	usage, err := fixture.db.ReviewCapUsage(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Total() != 5 {
		t.Fatalf("historical Review/repair total = %d, want the reported 5 of 3 sequence", usage.Total())
	}
	if got := reviewCapUsed(task, usage); got != 1 {
		t.Fatalf("Review cap used = %d, want 1 blocking-finding repair despite four Verdicts", got)
	}
	if got := reviewCapUsageDescription(task, reviewCapUsed(task, usage), 3); strings.Contains(got, "5 of 3") || !strings.Contains(got, "target-merge re-reviews") {
		t.Fatalf("Review cap Notice description = %q", got)
	}
	if got := reviewCapUsageDescription(task, 5, 3); !strings.Contains(got, "excess is from prior or User-authorized fixes") {
		t.Fatalf("over-cap Notice description does not explain the excess: %q", got)
	}
	if got := reviewCapRule(task, 3); !strings.Contains(got, "re-reviews") || !strings.Contains(got, "blocking findings") {
		t.Fatalf("Review cap rule = %q, want it to explain counted repairs and excluded re-reviews", got)
	}
}
