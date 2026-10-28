package app

import (
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func publishMergedRemoteBranch(t *testing.T, fixture *prMergeFixture) {
	t.Helper()
	fixture.task.PRURL = "https://github.com/acme/shop/pull/17"
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET pr_url=? WHERE id=?`, fixture.task.PRURL, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	gitTest(t, fixture.root, "--git-dir", fixture.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	t.Setenv("POSSE_TEST_GH_VIEW_STATE", "MERGED")
}

func remoteTaskBranch(t *testing.T, fixture *prMergeFixture) string {
	t.Helper()
	return strings.TrimSpace(gitTest(t, fixture.repo, "ls-remote", "--heads", "origin", "refs/heads/posse/t1"))
}

func TestFailedRemoteBranchDeletionRecordsCauseAndWatchRetries(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	publishMergedRemoteBranch(t, fixture)
	ctx := context.Background()
	t.Setenv("POSSE_TEST_GH_VIEW_FAIL", "HTTP 502: Bad Gateway")

	removed, cause, err := cleanUpMergedRemoteBranch(ctx, fixture.db, fixture.project, fixture.task, "", config.Config{}, fixture.headSHA)
	if err != nil || removed || !strings.Contains(cause, "HTTP 502: Bad Gateway") {
		t.Fatalf("failed deletion = removed %t cause %q err %v; want its forge cause", removed, cause, err)
	}
	cleanups, err := fixture.db.RemoteBranchCleanups(ctx, fixture.project.ID)
	if err != nil || len(cleanups) != 1 || cleanups[0].TaskID != fixture.task.ID || cleanups[0].HeadSHA != fixture.headSHA || cleanups[0].Cause != cause || cleanups[0].Attempts != 1 {
		t.Fatalf("recorded cleanups = %+v, %v", cleanups, err)
	}
	if remoteTaskBranch(t, fixture) == "" {
		t.Fatal("failed deletion removed the remote branch")
	}

	t.Setenv("POSSE_TEST_GH_VIEW_FAIL", "")
	if err := retryRemoteBranchCleanups(ctx, fixture.db, fixture.project, config.Config{}); err != nil {
		t.Fatal(err)
	}
	if remoteTaskBranch(t, fixture) == "" {
		t.Fatal("watch pass retried before the backoff elapsed")
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE remote_branch_cleanups SET failed_at=0`); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.maintainProjectWatch(ctx, fixture.db, fixture.project); err != nil {
		t.Fatalf("watch pass: %v", err)
	}
	if remote := remoteTaskBranch(t, fixture); remote != "" {
		t.Fatalf("watch pass did not retry the deletion: %s", remote)
	}
	if cleanups, err := fixture.db.RemoteBranchCleanups(ctx, fixture.project.ID); err != nil || len(cleanups) != 0 {
		t.Fatalf("settled cleanup remains recorded: %+v, %v", cleanups, err)
	}
}

func TestPoolPruneRemovesRemoteBranchesMergedAtTheirHead(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateDone)
	publishMergedRemoteBranch(t, fixture)
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET state='torn-down' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POSSE_TEST_GH_VIEW_FAIL", "HTTP 502: Bad Gateway")
	if code, output, stderr := fixture.run("pool", "prune", "--yes"); code != 0 || !strings.Contains(output, "remote-branch") || !strings.Contains(output, "HTTP 502: Bad Gateway") || remoteTaskBranch(t, fixture) == "" {
		t.Fatalf("unverified remote branch prune: %d %s %s", code, output, stderr)
	}
	t.Setenv("POSSE_TEST_GH_VIEW_FAIL", "")

	merged := fixture.headSHA
	gitTest(t, fixture.worktree, "commit", "--allow-empty", "-m", "advance after merge")
	gitTest(t, fixture.worktree, "push", "origin", "refs/heads/posse/t1:refs/heads/posse/t1")
	if code, output, stderr := fixture.run("pool", "prune"); code != 0 || strings.Contains(output, "remote-branch") {
		t.Fatalf("prune listed a remote branch past its merged PR head: %d %s %s", code, output, stderr)
	}

	gitTest(t, fixture.worktree, "push", "--force", "origin", merged+":refs/heads/posse/t1")
	code, output, stderr := fixture.run("pool", "prune")
	if code != 0 || !strings.Contains(output, "remote-branch") || !strings.Contains(output, "posse/t1") || !strings.Contains(output, "dry_run: true") {
		t.Fatalf("dry run did not list the merged remote branch: %d %s %s", code, output, stderr)
	}
	if remoteTaskBranch(t, fixture) == "" {
		t.Fatal("dry run removed the remote branch")
	}
	if code, output, stderr := fixture.run("pool", "prune", "--yes"); code != 0 || !strings.Contains(output, "remote-branch") {
		t.Fatalf("prune --yes: %d %s %s", code, output, stderr)
	}
	if remote := remoteTaskBranch(t, fixture); remote != "" {
		t.Fatalf("prune --yes kept the merged remote branch: %s", remote)
	}
}
