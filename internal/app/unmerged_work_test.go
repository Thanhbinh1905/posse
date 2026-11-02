package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

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
