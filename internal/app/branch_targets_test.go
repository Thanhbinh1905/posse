package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestSelectedBranchRefValidatesOriginBranchWithoutEffects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	initRepo(t, root)
	remote := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, remote, "init", "--bare", "-b", "main")
	gitTest(t, root, "remote", "add", "origin", remote)
	gitTest(t, root, "push", "origin", "main")
	gitTest(t, root, "branch", "staging")
	gitTest(t, root, "push", "origin", "staging")
	gitTest(t, root, "update-ref", "-d", "refs/remotes/origin/staging")

	ref, err := selectedBranchRef(context.Background(), root, "staging", "target")
	if err != nil || ref != "refs/remotes/origin/staging" {
		t.Fatalf("selected branch ref = %q, %v", ref, err)
	}
	if _, err := gitOutput(context.Background(), root, "rev-parse", "--verify", ref); err == nil {
		t.Fatal("branch validation created a remote-tracking ref before admission")
	}
	if _, err := selectedBranchRef(context.Background(), root, "missing", "target"); err == nil || !strings.Contains(err.Error(), "does not exist on origin") {
		t.Fatalf("missing origin branch error = %v", err)
	}
	if _, err := selectedBranchRef(context.Background(), root, "bad~name", "target"); err == nil || !strings.Contains(err.Error(), "not a valid Git branch name") {
		t.Fatalf("invalid branch name error = %v", err)
	}
}

func TestSyncTaskTargetFastForwardsTheFrozenNonDefaultBranch(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	baseSHA := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main"))
	gitTest(t, fixture.repo, "branch", "staging", "main")
	gitTest(t, fixture.repo, "push", "origin", "staging")
	gitTest(t, fixture.root, "clone", fixture.remote, filepath.Join(fixture.root, "upstream"))
	upstream := filepath.Join(fixture.root, "upstream")
	gitTest(t, upstream, "config", "user.name", "Test User")
	gitTest(t, upstream, "config", "user.email", "test@example.com")
	gitTest(t, upstream, "checkout", "staging")
	if err := os.WriteFile(filepath.Join(upstream, "staging.txt"), []byte("new target commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, upstream, "add", "staging.txt")
	gitTest(t, upstream, "commit", "-m", "advance staging")
	gitTest(t, upstream, "push", "origin", "staging")

	task := store.Task{TargetBranch: "staging"}
	result, err := fixture.service.syncTaskTarget(context.Background(), fixture.db, fixture.project, task)
	if err != nil || result.Status != "updated" {
		t.Fatalf("sync target result = %+v, %v", result, err)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/staging")); got != strings.TrimSpace(gitTest(t, upstream, "rev-parse", "staging")) {
		t.Fatalf("local staging tip = %s, want remote staging tip", got)
	}
	if got := strings.TrimSpace(gitTest(t, fixture.repo, "rev-parse", "refs/heads/main")); got != baseSHA {
		t.Fatalf("syncing staging changed default branch to %s", got)
	}
}

func TestLocalMergeUsesFrozenNonDefaultTarget(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "worktree")
	initRepo(t, repo)
	gitTest(t, repo, "branch", "staging")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", worktree, "refs/heads/staging")
	if err := os.WriteFile(filepath.Join(worktree, "change.txt"), []byte("staging change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, worktree, "add", "change.txt")
	gitTest(t, worktree, "commit", "-m", "staging change")
	gatedSHA := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD"))

	project := store.Project{Root: repo, DefaultBranch: "main"}
	task := store.Task{Branch: "posse/t1", TargetBranch: "staging", WorktreePath: worktree, GatedSHA: gatedSHA}
	if err := verifyFastForward(context.Background(), project, task); err != nil {
		t.Fatalf("verify selected target: %v", err)
	}
	if err := mergeLocal(context.Background(), project, task); err != nil {
		t.Fatalf("merge selected target: %v", err)
	}
	if got := strings.TrimSpace(gitTest(t, repo, "rev-parse", "refs/heads/staging")); got != gatedSHA {
		t.Fatalf("staging tip = %s, want Task tip %s", got, gatedSHA)
	}
	if _, err := gitOutput(context.Background(), repo, "cat-file", "-e", "refs/heads/main:change.txt"); err == nil {
		t.Fatal("local Merge changed the Project default branch instead of staging")
	}
}
