//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
)

func failReviewIdentityTask(t *testing.T, fixture *prLifecycleFixture, taskID string) {
	t.Helper()
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET state='failed' WHERE seq=? AND state='working'`, strings.TrimPrefix(taskID, "t")); err != nil {
		t.Fatal(err)
	}
}

func TestFailedReviewTaskTearsDownWithoutDiscard(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := startReviewIdentityTask(t, fixture, "Author", "author", ""); code != 0 {
		t.Fatalf("start author Task: %s", output)
	}
	if code, output := startReviewIdentityTask(t, fixture, "Failing review", "reviewer", "t1"); code != 0 {
		t.Fatalf("start Review Task: %s", output)
	}
	failReviewIdentityTask(t, fixture, "t2")
	if code, output := runReviewIdentityCLI(t, fixture, "teardown", "t2"); code != 0 || !strings.Contains(output, "torn-down") {
		t.Fatalf("teardown of failed Review Task without --discard: code=%d %s", code, output)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t2")
	if err != nil || string(task.State) != "torn-down" {
		t.Fatalf("failed Review Task state after teardown = %q, %v", task.State, err)
	}
}

func TestFailedChangeTaskTeardownStillRequiresDiscard(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := startReviewIdentityTask(t, fixture, "Failing author", "author", ""); code != 0 {
		t.Fatalf("start author Task: %s", output)
	}
	failReviewIdentityTask(t, fixture, "t1")
	if code, output := runReviewIdentityCLI(t, fixture, "teardown", "t1"); code == 0 || !strings.Contains(output, "teardown_refused") {
		t.Fatalf("teardown of failed Change Task without --discard: code=%d %s", code, output)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || string(task.State) != "failed" {
		t.Fatalf("failed Change Task state after refused teardown = %q, %v", task.State, err)
	}
}
