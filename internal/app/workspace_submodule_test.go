package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceSandboxRepairChecksMemberWorkBeforeReset(t *testing.T) {
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\n", []string{"worker"}, []string{"worker"})
	ctx := context.Background()
	sandbox, err := f.db.SandboxByTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.BreakSandbox(ctx, sandbox.ID, f.task.ID); err != nil {
		t.Fatal(err)
	}
	unsaved := filepath.Join(sandbox.Path, "worker", "unsaved.txt")
	if err := os.WriteFile(unsaved, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("pool", "repair", "sandbox-1"); code == 0 || !strings.Contains(f.out.String(), "unsaved.txt") {
		t.Fatalf("repair result = %d, %s; want exact unsaved path", code, f.out.String())
	}
	if err := os.Remove(unsaved); err != nil {
		t.Fatal(err)
	}
	if code := f.run("pool", "repair", "sandbox-1"); code != 0 {
		t.Fatalf("repair failed: %d %s", code, f.out.String())
	}
	sandboxes, err := f.db.Sandboxes(ctx, f.project.ID)
	if err != nil || len(sandboxes) != 1 || sandboxes[0].State != "idle" {
		t.Fatalf("Sandboxes after repair = %#v, %v", sandboxes, err)
	}
}

func TestReleaseWorkspaceSandboxResetsSubmoduleGitlinkDrift(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	f := newWorkspaceMergeFixture(t, "[defaults]\nmerge_mode = \"local\"\n", []string{"worker"}, []string{"worker"})
	memberRoot := filepath.Join(f.workspace, "worker")
	submodule := filepath.Join(f.root, "submodule")
	submoduleRemote := filepath.Join(f.root, "submodule.git")
	gitTest(t, f.root, "init", "--bare", "-b", "main", submoduleRemote)
	initRepo(t, submodule)
	gitTest(t, submodule, "remote", "add", "origin", submoduleRemote)
	if err := os.WriteFile(filepath.Join(submodule, "tracked.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, submodule, "add", "tracked.txt")
	gitTest(t, submodule, "commit", "-m", "submodule initial")
	gitTest(t, submodule, "push", "-u", "origin", "main")
	gitTest(t, memberRoot, "config", "protocol.file.allow", "always")
	gitTest(t, memberRoot, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "vendor/sub")
	gitTest(t, memberRoot, "commit", "-m", "add submodule")
	gitTest(t, memberRoot, "push", "origin", "main")
	worktree := filepath.Join(f.task.WorktreePath, "worker")
	if err := resetWorktree(ctx, worktree, repoTarget{Root: memberRoot, DefaultBranch: "main"}, "warm", false); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "submodule", "update", "--init", "--recursive")
	unsaved := filepath.Join(worktree, "vendor", "sub", "unsaved.txt")
	if err := os.WriteFile(unsaved, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := releaseWorkspaceSandbox(ctx, f.db, f.project, store.Sandbox{Path: f.task.WorktreePath, TaskID: f.task.ID}, "warm", removeGitWorktree); err == nil || !strings.Contains(err.Error(), "vendor/sub") {
		t.Fatalf("release error = %v, want unsaved submodule path vendor/sub", err)
	}
	if err := os.Remove(unsaved); err != nil {
		t.Fatal(err)
	}

	gitTest(t, submodule, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "advance submodule")
	gitTest(t, submodule, "push", "origin", "main")
	gitTest(t, worktree, "-C", "vendor/sub", "fetch", "origin")
	gitTest(t, worktree, "-C", "vendor/sub", "checkout", "--detach", "FETCH_HEAD")
	if !sandboxWorktreeClean(ctx, worktree) {
		t.Fatal("submodule gitlink drift alone made the Member dirty")
	}
	want := strings.TrimSpace(gitTest(t, memberRoot, "rev-parse", "HEAD:vendor/sub"))

	if err := releaseWorkspaceSandbox(ctx, f.db, f.project, store.Sandbox{Path: f.task.WorktreePath, TaskID: f.task.ID}, "warm", removeGitWorktree); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(gitTest(t, worktree, "-C", "vendor/sub", "rev-parse", "HEAD"))
	if got != want {
		t.Fatalf("submodule HEAD = %s, want recorded gitlink %s", got, want)
	}
}
