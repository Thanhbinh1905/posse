package app

import (
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A target move under a done full-pipeline PR stales its validation, so
// Posse queues the refresh to the Worker itself; the Lead sends nothing.
func TestPRWatchQueuesFullPipelineFreshnessWhenTargetMoves(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	installValidationPlanForReview(t, fixture, "full-pipeline")
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	targetTip := fixture.advanceTarget(t, false, true)
	fixture.setGraphQLState(t, "OPEN", "SUCCESS", "APPROVED", "MERGEABLE", "", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := fixture.service.pollProjectPullRequests(ctx, fixture.db, fixture.project, cfg, true); err != nil {
			t.Fatal(err)
		}
	}
	var rounds int
	var expectedHead string
	if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(expected_base_head),'') FROM validation_fix_rounds WHERE ship_task_id=? AND kind='freshness'`, fixture.task.ID).Scan(&rounds, &expectedHead); err != nil || rounds != 1 || expectedHead != fixture.headSHA {
		t.Fatalf("freshness rounds=%d expected head=%s err=%v; want one for %s", rounds, expectedHead, err, fixture.headSHA)
	}
	var body string
	if err := fixture.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE task_id=? AND status='queued'`, fixture.task.ID).Scan(&body); err != nil {
		t.Fatalf("no queued freshness instruction: %v", err)
	}
	for _, want := range []string{targetTip, "posse publish", "posse validate test", "posse validate docs", "posse validate lint", "candidate checkout"} {
		if !strings.Contains(body, want) {
			t.Fatalf("full-pipeline freshness instruction lacks %q: %s", want, body)
		}
	}
}
