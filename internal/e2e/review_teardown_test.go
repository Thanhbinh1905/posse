//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func endReviewIdentityTask(t *testing.T, fixture *prLifecycleFixture, taskID, state string) {
	t.Helper()
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET state=? WHERE seq=? AND state='working'`, state, strings.TrimPrefix(taskID, "t")); err != nil {
		t.Fatal(err)
	}
}

func TestEndedReviewTaskTeardownPreservesFindingsAndReport(t *testing.T) {
	for _, state := range []string{"failed", "lost"} {
		t.Run(state, func(t *testing.T) {
			fixture := reviewIdentityFixture(t)
			if code, output := startReviewIdentityTask(t, fixture, "Author", "author", ""); code != 0 {
				t.Fatalf("start author Task: %s", output)
			}
			if code, output := startReviewIdentityTask(t, fixture, "Review", "reviewer", "t1"); code != 0 {
				t.Fatalf("start Review Task: %s", output)
			}
			artifactDir := filepath.Join(fixture.home, "projects", fixture.project.Name, "tasks", "t2")
			if err := os.MkdirAll(artifactDir, 0o700); err != nil {
				t.Fatal(err)
			}
			artifacts := map[string]string{
				"findings.json": `{"schema":"posse.review-findings.v1","patterns":[],"resolutions":[]}`,
				"report.md":     "Review evidence.\n",
			}
			for name, contents := range artifacts {
				if err := os.WriteFile(filepath.Join(artifactDir, name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			endReviewIdentityTask(t, fixture, "t2", state)
			if code, output := runReviewIdentityCLI(t, fixture, "teardown", "t2"); code != 0 || !strings.Contains(output, "torn-down") {
				t.Fatalf("teardown of %s Review Task without --discard: code=%d %s", state, code, output)
			}
			task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t2")
			if err != nil || string(task.State) != "torn-down" {
				t.Fatalf("%s Review Task state after teardown = %q, %v", state, task.State, err)
			}
			for name, want := range artifacts {
				contents, err := os.ReadFile(filepath.Join(artifactDir, name))
				if err != nil || string(contents) != want {
					t.Errorf("%s after %s Review Task teardown = %q, %v; want %q", name, state, contents, err, want)
				}
			}
		})
	}
}

func TestFailedChangeTaskTeardownStillRequiresDiscard(t *testing.T) {
	fixture := reviewIdentityFixture(t)
	if code, output := startReviewIdentityTask(t, fixture, "Failing author", "author", ""); code != 0 {
		t.Fatalf("start author Task: %s", output)
	}
	endReviewIdentityTask(t, fixture, "t1", "failed")
	if code, output := runReviewIdentityCLI(t, fixture, "teardown", "t1"); code == 0 || !strings.Contains(output, "teardown_refused") {
		t.Fatalf("teardown of failed Change Task without --discard: code=%d %s", code, output)
	}
	task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil || string(task.State) != "failed" {
		t.Fatalf("failed Change Task state after refused teardown = %q, %v", task.State, err)
	}
}
