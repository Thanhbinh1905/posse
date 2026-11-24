package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTeardownAllowsLocalBranchAlreadyContainedInTargetE2E(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateDone)
	attachPRFixtureSandbox(t, fixture)
	gitTest(t, fixture.repo, "branch", "release", "main")
	target := filepath.Join(fixture.root, "target")
	gitTest(t, fixture.repo, "worktree", "add", target, "release")
	gitTest(t, target, "merge", "--ff-only", "refs/heads/posse/t1")
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET target_branch='release' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	removeFixtureTaskPane(fixture)

	code, output, stderr := fixture.run("teardown", "t1")
	if code != 0 || !strings.Contains(output, "state: torn-down") {
		t.Fatalf("plain teardown of a local branch already in its target = %d, output=%s stderr=%s", code, output, stderr)
	}
	updated, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil || updated.State != store.StateTornDown {
		t.Fatalf("Task after plain teardown = %#v, %v", updated, err)
	}
}

func TestTeardownAllowsLocalBranchAlreadyContainedInRemoteTargetE2E(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateDone)
	attachPRFixtureSandbox(t, fixture)
	gitTest(t, fixture.repo, "push", "origin", "posse/t1:refs/heads/release")
	gitTest(t, fixture.repo, "fetch", "origin", "refs/heads/release:refs/remotes/origin/release")
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET target_branch='release' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	removeFixtureTaskPane(fixture)

	code, output, stderr := fixture.run("teardown", "t1")
	if code != 0 || !strings.Contains(output, "state: torn-down") {
		t.Fatalf("plain teardown of a local branch already in its remote-tracking target = %d, output=%s stderr=%s", code, output, stderr)
	}
}

func TestMergedLocalTaskHasNoUnmergedWork(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	sandbox := filepath.Join(root, "sandbox")
	home := filepath.Join(root, "posse")
	initRepo(t, repo)
	gitTest(t, repo, "branch", "release", "main")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", sandbox, "main")
	if err := os.WriteFile(filepath.Join(sandbox, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, sandbox, "add", "change.txt")
	gitTest(t, sandbox, "commit", "-m", "worker change")
	target := filepath.Join(root, "target")
	gitTest(t, repo, "worktree", "add", target, "release")
	gitTest(t, target, "merge", "--ff-only", "posse/t1")

	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Already merged",
		MergeMode: "local", Branch: "posse/t1", BaseRef: "main", TargetBranch: "release", WorktreePath: sandbox,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateDone, "worker", "finished"); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if work, err := testService(home, nil).unmergedWork(ctx, db, project, task); err != nil || work != "" {
		t.Fatalf("merged local Task still has unmerged work: work=%q err=%v", work, err)
	}
}

func TestNoChangeSignalAndTeardownUseOriginWhenLocalDefaultBranchIsBehind(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "origin.git")
	sandbox := filepath.Join(root, "sandbox")
	home := filepath.Join(root, "posse")
	initRepo(t, repo)
	gitTest(t, root, "init", "--bare", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "-u", "origin", "main")
	gitTest(t, repo, "fetch", "origin")

	advance := filepath.Join(root, "advance")
	gitTest(t, repo, "worktree", "add", "-b", "advance-origin", advance, "main")
	if err := os.WriteFile(filepath.Join(advance, "origin-advance.txt"), []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, advance, "add", "origin-advance.txt")
	gitTest(t, advance, "commit", "-m", "advance origin main")
	gitTest(t, advance, "push", "origin", "HEAD:main")
	gitTest(t, repo, "fetch", "origin")
	gitTest(t, repo, "worktree", "add", "-b", "posse/t1", sandbox, "refs/remotes/origin/main")

	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Already satisfied", State: store.StateSpawning,
		MergeMode: "local", Branch: "posse/t1", BaseRef: "main", WorktreePath: sandbox,
		HerdrWorkspaceID: "w2", PaneID: "w2:p1", PaneLabel: "posse:shop:t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "Worker started"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", "w2:p1")
	t.Chdir(sandbox)

	var output bytes.Buffer
	cli := testService(home, nil).CLI()
	cli.Out, cli.ErrOut = &output, &output
	if code := cli.Run([]string{"signal", "done", "--no-change", "Already fixed on origin/main"}); code != 0 {
		t.Fatalf("no-change Signal failed: code=%d output=%s", code, output.String())
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil || task.State != store.StateDone {
		t.Fatalf("no-change Signal left Task in %s: %v", task.State, err)
	}
	if work, err := testService(home, nil).unmergedWork(ctx, db, project, task); err != nil || work != "" {
		t.Fatalf("teardown considered an origin/main Task unmerged: work=%q err=%v", work, err)
	}
}
