package app

import (
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

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
