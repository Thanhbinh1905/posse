//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func Test014TaskSnapshotRebuildPreservesMergeAuthorityAndSandbox(t *testing.T) {
	f := newPRLifecycleFixture(t)
	stopBackgroundWatch(t, f)
	t.Cleanup(func() {
		if f.db != nil {
			_ = f.db.Close()
		}
	})
	legacyPath := filepath.Join(f.home, "remuda", f.project.Name, "mount-17")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, f.repo, "worktree", "add", "-b", "posse/historical-recovery", legacyPath, "main")
	fixture, err := os.ReadFile(filepath.Join(moduleRoot(t), "internal", "store", "testdata", "task-v014.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// This fixture names the 0.14 persisted keys literally. Do not generate it
	// through the current Task encoder, which would hide a renamed-key bug.
	original := strings.NewReplacer(
		strconv.Quote("__PROJECT_UUID__"), strconv.Quote(f.project.UUID),
		strconv.Quote("__PROJECT_ROOT__"), strconv.Quote(f.repo),
		strconv.Quote("__WORKTREE_PATH__"), strconv.Quote(legacyPath),
	).Replace(string(fixture))
	path := f.db.TaskSnapshotPath(f.project.Name, 1)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f.db = nil
	// Recover from Task snapshots alone, with the SQLite database lost.
	for _, path := range []string{filepath.Join(f.home, "posse.db"), filepath.Join(f.home, "projects", f.project.Name, "project.toml")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	userEnv := setEnv(f.env, "HERDR_PANE_ID", "")
	userEnv = setEnv(userEnv, "HERDR_WORKSPACE_ID", "")
	userEnv = setEnv(userEnv, "HERDR_ENV", "")
	command := exec.Command(f.binary, "recover", "--rebuild")
	command.Dir, command.Env = f.repo, userEnv
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("0.14 snapshot recovery: %v\n%s", err, output)
	}
	f.db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.MergeMode != "pr" || task.AutonomyMerge != "ask" || task.SandboxID != 17 || task.MergedRef != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("silent loss of 0.14 persisted fields after recover --rebuild: %#v", task)
	}
	if task.ID != 41 || task.ProjectID != f.project.ID || task.Type != "change" || task.State != store.StateDone || task.Title != `User said "Rider must land"` || task.WorktreePath != legacyPath || task.PRURL != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("recovery changed Task identity or evidence: %#v", task)
	}
	sandbox, err := f.db.SandboxByTask(context.Background(), task.ID)
	if err != nil || sandbox.ID != task.SandboxID || sandbox.TaskID != task.ID || sandbox.State != "held" || sandbox.Path != legacyPath {
		t.Fatalf("recovery lost Sandbox custody: %#v %v", sandbox, err)
	}
}
