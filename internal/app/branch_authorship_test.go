package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestBranchAuthorHandoffBundleCapturesScopeAndValidation(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	taskDir := filepath.Join(fixture.home, "projects", fixture.project.Name, "tasks", "t1")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const scope = "Done when the stale writer is fenced.\n"
	if err := os.WriteFile(filepath.Join(taskDir, "launch.md"), []byte(scope), 0o600); err != nil {
		t.Fatal(err)
	}
	repository, err := gitCommonDir(context.Background(), fixture.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
		t.Fatal(err)
	}
	bundle, err := buildBranchAuthorHandoffBundle(context.Background(), fixture.db, fixture.home, fixture.project.Name, fixture.task, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Attempt    int                           `json:"attempt"`
		Generation int64                         `json:"generation"`
		Scope      string                        `json:"scope"`
		Findings   []store.ReviewFindingDetail   `json:"findings"`
		Candidates []store.ValidationCandidate   `json:"candidates"`
		Evidence   []store.ValidationStageResult `json:"evidence"`
	}
	if err := json.Unmarshal(bundle, &decoded); err != nil {
		t.Fatalf("decode handoff bundle: %v", err)
	}
	if decoded.Attempt != 1 || decoded.Generation != 1 || decoded.Scope != scope {
		t.Fatalf("handoff identity or scope = %+v", decoded)
	}
	if decoded.Findings == nil || decoded.Candidates == nil || decoded.Evidence == nil {
		t.Fatalf("handoff omitted an empty findings/candidate/evidence section: %s", bundle)
	}
}

func TestBranchAuthorHeadUpdateRejectsRewind(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET launches=1 WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	fixture.task.Launches = 1
	repository, err := gitCommonDir(context.Background(), fixture.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(fixture.worktree, "change.txt")
	if err := os.WriteFile(file, []byte("advance the claimed head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.worktree, "add", "change.txt")
	gitTest(t, fixture.worktree, "commit", "-m", "advance author head")
	head, err := gitOutput(context.Background(), fixture.worktree, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TASK_GENERATION", "1")
	if err := updateWorkerBranchAuthorHead(context.Background(), fixture.db, fixture.task, head); err != nil {
		t.Fatalf("record descendant head: %v", err)
	}
	gitTest(t, fixture.worktree, "reset", "--hard", fixture.headSHA)
	if err := updateWorkerBranchAuthorHead(context.Background(), fixture.db, fixture.task, fixture.headSHA); !errors.Is(err, store.ErrBranchAuthorHeadChanged) {
		t.Fatalf("rewound branch author head update error = %v, want ErrBranchAuthorHeadChanged", err)
	}
	claim, err := fixture.db.BranchAuthor(context.Background(), repository, fixture.task.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if claim.HeadSHA != head {
		t.Fatalf("rewind changed recorded author head to %s, want %s", claim.HeadSHA, head)
	}
}

func TestBranchAuthorTransferWaitsForActiveOperation(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	repository, err := gitCommonDir(context.Background(), fixture.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireTaskBranchAuthorLock(context.Background(), fixture.worktree, fixture.task.Branch)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := claimTaskBranchAuthor(ctx, fixture.db, fixture.task, 2, true); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("author transfer during active publish/merge operation error = %v, want busy", err)
	}
	claim, err := fixture.db.BranchAuthor(context.Background(), repository, fixture.task.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Generation != 1 || claim.Attempt != 1 {
		t.Fatalf("active author operation lost custody: %+v", claim)
	}
}

func TestDoneSignalRefusesStaleBranchAuthorGeneration(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	repository, err := gitCommonDir(context.Background(), fixture.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 2, fixture.headSHA, fixture.headSHA, true); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET launches=2 WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	fixture.task.Launches = 2
	turn, err := fixture.db.TaskTurnToken(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_WORKER_HOME", fixture.home)
	t.Setenv("POSSE_TASK_GENERATION", "1")
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	code, output, stderr := fixture.run("signal", "done", "Stale Worker", "--turn", turn)
	if code == 0 || !strings.Contains(output+stderr, "branch author claim") {
		t.Fatalf("stale author done Signal was not refused: code=%d output=%s stderr=%s", code, output, stderr)
	}
	var signals int
	if err := fixture.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM signals WHERE task_id=? AND verb='done'`, fixture.task.ID).Scan(&signals); err != nil {
		t.Fatal(err)
	}
	if signals != 0 {
		t.Fatalf("stale author recorded %d done Signals", signals)
	}
}

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
	if _, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 1, fixture.headSHA, fixture.headSHA, false); err != nil {
		t.Fatal(err)
	}
	claim, err := fixture.db.ClaimBranchAuthor(context.Background(), repository, fixture.task.Branch, fixture.task.ID, 2, fixture.headSHA, fixture.headSHA, true)
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
