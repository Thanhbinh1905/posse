package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A missing default branch is a repository precondition. It must refuse before
// admission instead of breaking every Sandbox and failing the Task (#699).
func TestStartRefusesMissingDefaultBranchBeforeAdmission(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	gitTest(t, f.repo, "checkout", "-q", "-b", "other")
	gitTest(t, f.repo, "branch", "-D", "main")
	output := outcomeCLI(t, f.service, 1, "start", "--brief", f.brief, "--name", "no-main")
	if !strings.Contains(output, "default_branch_missing") || !strings.Contains(output, "git branch main <commit>") {
		t.Fatalf("start did not name the missing default branch and its fix: %s", output)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("refused start left Tasks %#v (%v)", tasks, err)
	}
	sandboxes, err := f.db.Sandboxes(context.Background(), f.project.ID)
	if err != nil || len(sandboxes) != 0 {
		t.Fatalf("refused start touched Sandboxes %#v (%v)", sandboxes, err)
	}
}

func TestStartUsesFreshOriginDefaultBranchWhenProjectCheckoutIsBehind(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	origin := filepath.Join(f.root, "origin.git")
	gitTest(t, f.root, "init", "-q", "--bare", origin)
	gitTest(t, f.repo, "remote", "add", "origin", origin)
	gitTest(t, f.repo, "push", "-q", "origin", "main")
	gitTest(t, f.repo, "fetch", "-q", "origin")
	advance := filepath.Join(f.root, "advance")
	gitTest(t, f.repo, "worktree", "add", "-q", "-b", "advance-origin", advance, "main")
	if err := os.WriteFile(filepath.Join(advance, "origin-advance.txt"), []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, advance, "add", "origin-advance.txt")
	gitTest(t, advance, "commit", "-q", "-m", "advance origin main")
	gitTest(t, advance, "push", "-q", "origin", "HEAD:main")
	output := outcomeCLI(t, f.service, 0, "start", "--brief", f.brief, "--name", "fresh-origin")
	if strings.Contains(output, "default_branch_missing") {
		t.Fatalf("start refused the fresh origin default branch: %s", output)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("start did not admit one Task: %#v (%v)", tasks, err)
	}
	got := gitTest(t, tasks[0].WorktreePath, "rev-parse", "HEAD")
	want := gitTest(t, f.repo, "ls-remote", "origin", "refs/heads/main")
	if fields := strings.Fields(want); len(fields) != 2 || strings.TrimSpace(got) != fields[0] {
		t.Fatalf("Task branch starts at %s, want the fresh origin/main tip %q", got, want)
	}
}

// A default branch that exists only on origin still starts Tasks (#702).
func TestStartAcceptsOriginOnlyDefaultBranch(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	origin := filepath.Join(f.root, "origin.git")
	gitTest(t, f.root, "init", "-q", "--bare", origin)
	gitTest(t, f.repo, "remote", "add", "origin", origin)
	gitTest(t, f.repo, "push", "-q", "origin", "main")
	gitTest(t, f.repo, "fetch", "-q", "origin")
	gitTest(t, f.repo, "checkout", "-q", "-b", "other")
	gitTest(t, f.repo, "branch", "-D", "main")
	output := outcomeCLI(t, f.service, 0, "start", "--brief", f.brief, "--name", "origin-main")
	if strings.Contains(output, "default_branch_missing") {
		t.Fatalf("start refused an origin-only default branch: %s", output)
	}
	tasks, err := f.db.Tasks(context.Background(), f.project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("start did not admit one Task: %#v (%v)", tasks, err)
	}
	head := gitTest(t, tasks[0].WorktreePath, "rev-parse", "HEAD")
	if want := gitTest(t, f.repo, "rev-parse", "refs/remotes/origin/main"); head != want {
		t.Fatalf("Task branch starts at %s, want origin/main %s", head, want)
	}
}

// A base ref that does not exist is an error, not a count of every commit on
// HEAD; only a default branch with no commit yet counts every commit (#702).
func TestCommitsAheadOfBaseRefusesMissingBase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo, worktree := filepath.Join(root, "repo"), filepath.Join(root, "sandbox")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "-q", "-b", "posse/t1", worktree)
	if count, err := commitsAheadOfBase(ctx, worktree, "gone"); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("missing base counted %q commits ahead (%v)", count, err)
	}

	unborn, sandbox := filepath.Join(root, "unborn"), filepath.Join(root, "unborn-sandbox")
	gitTest(t, root, "init", "-q", "-b", "main", unborn)
	gitTest(t, unborn, "worktree", "add", "-q", "--orphan", "-b", "posse/t2", sandbox)
	gitTest(t, sandbox, "-c", "user.name=Posse Test", "-c", "user.email=posse@example.test", "commit", "-q", "--allow-empty", "-m", "first")
	if count, err := commitsAheadOfBase(ctx, sandbox, "main"); err != nil || count != "1" {
		t.Fatalf("unborn default branch: %q commits ahead (%v), want 1", count, err)
	}
}
