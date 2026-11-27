package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkerDependencyResumeStillRequiresCIAndReview(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateWorking)
	ctx := t.Context()
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("[defaults]\nmerge_mode = \"pr\"\nreview = \"quick\"\nauto_teardown = \"never\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const url = "https://github.com/acme/shop/pull/17"
	if code, out, stderr := f.run("publish", "Worker change"); code != 0 {
		t.Fatalf("publish: %d %s %s", code, out, stderr)
	}
	if err := f.db.UpdateTaskMerge(ctx, f.task.ID, url, ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, f.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.setGraphQLState(t, "OPEN", "FAILURE", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
	if err := f.service.pollProjectPullRequests(ctx, f.db, f.project, cfg, true); err != nil {
		t.Fatal(err)
	}
	done := []string{"signal", "done", "Worker change", "--pr", url}
	if code, out, _ := f.run(done...); code != 1 || !strings.Contains(out, "checks_failed") {
		t.Fatalf("done with failed checks before parking: %d %s", code, out)
	}

	dep, err := f.db.CreateTask(ctx, f.project.ID, store.Task{Seq: 2, Type: "change", Title: "Fix the CI check"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, dep, store.StateSpawning, store.StateWorking, "cli", "Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, dep, store.StateWorking, store.StateDone, "worker", "CI fix ready"); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := f.run("signal", "blocked", "Waiting for the CI fix", "--on", "t2"); code != 0 {
		t.Fatalf("park after checks_failed: %d %s %s", code, out, stderr)
	}
	if err := f.db.Transition(ctx, dep, store.StateDone, store.StateMerged, "cli", "CI fix landed"); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := f.run("show", "t1"); code != 0 {
		t.Fatalf("show after landing: %d %s %s", code, out, stderr)
	}
	turn, err := f.db.TaskTurnToken(ctx, f.task.ID)
	if err != nil || !strings.HasPrefix(turn, "message:") {
		t.Fatalf("resume turn = %q, err = %v", turn, err)
	}
	done = append(done, "--turn", turn)
	if code, out, _ := f.run(done...); code != 1 || !strings.Contains(out, "checks_failed") {
		t.Fatalf("done bypassed failed checks after parking: %d %s", code, out)
	}
	f.setGraphQLState(t, "OPEN", "SUCCESS", "REVIEW_REQUIRED", "MERGEABLE", "", f.headSHA)
	if code, out, stderr := f.run(done...); code != 0 {
		t.Fatalf("done with passing checks: %d %s %s", code, out, stderr)
	}
	if code, out, _ := f.run("merge", "t1", "--execute", "--user-approved", "User approved merging this change"); code != 1 || !strings.Contains(out, "review_required") {
		t.Fatalf("merge bypassed required review: %d %s", code, out)
	}
}
