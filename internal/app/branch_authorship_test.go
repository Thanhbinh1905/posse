package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishRefusesStaleBranchAuthorGeneration(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", "working")
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET launches=2 WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	fixture.task.Launches = 2
	repository, err := gitCommonDir(context.Background(), fixture.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, false); err != nil {
		t.Fatal(err)
	}
	claim, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 2, fixture.headSHA, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TASK_GENERATION", "1")
	code, output, stderr := fixture.run("publish", "Stale author", "--verify", "go test ./internal/app -> passed")
	if code == 0 || !strings.Contains(output+stderr, "current branch author claim") {
		t.Fatalf("stale author publish was not refused: code=%d output=%s stderr=%s", code, output, stderr)
	}
	if _, err := os.Stat(fixture.ghLog); !os.IsNotExist(err) {
		if err != nil {
			t.Fatal(err)
		}
		if calls := readFileForPublishTest(t, fixture.ghLog); calls != "" {
			t.Fatalf("stale author reached the forge: %s", calls)
		}
	}
	if claim.Generation != 2 {
		t.Fatalf("claim generation = %d, want 2", claim.Generation)
	}
	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	var guardErr strings.Builder
	guardInput := `{"tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(fixture.worktree, "change.txt") + `"}}`
	if code := fixture.service.RunGuard(strings.NewReader(guardInput), &guardErr); code != guardExitBlocked || !strings.Contains(guardErr.String(), "branch author claim") {
		t.Fatalf("stale author write guard = %d, %s", code, guardErr.String())
	}
	t.Setenv("POSSE_TASK_GENERATION", "")
	guardErr.Reset()
	if code := fixture.service.RunGuard(strings.NewReader(guardInput), &guardErr); code != guardExitBlocked || !strings.Contains(guardErr.String(), "branch author claim") {
		t.Fatalf("generation-free author write guard = %d, %s", code, guardErr.String())
	}
	t.Setenv("POSSE_TASK_GENERATION", "2")
	code, output, stderr = fixture.run("publish", "Current author", "--verify", "go test ./internal/app -> passed")
	if code != 0 || !strings.Contains(output, "https://github.com/acme/shop/pull/17") {
		t.Fatalf("current author publish failed: code=%d output=%s stderr=%s", code, output, stderr)
	}
}
