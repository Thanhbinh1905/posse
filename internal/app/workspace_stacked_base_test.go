package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceTaskStartsFromAnotherPublishedTaskBranch(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceFixture(t, root)
	workerRoot := filepath.Join(workspace, "worker")
	gitTest(t, workerRoot, "branch", "staging", "main")
	gitTest(t, workerRoot, "push", "origin", "staging")
	gitTest(t, workerRoot, "checkout", "-b", "posse/lower", "main")
	if err := os.WriteFile(filepath.Join(workerRoot, "lower.txt"), []byte("lower Task commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, workerRoot, "add", "lower.txt")
	gitTest(t, workerRoot, "commit", "-m", "lower Task commit")
	gitTest(t, workerRoot, "push", "origin", "posse/lower")
	lowerSHA := strings.TrimSpace(gitTest(t, workerRoot, "rev-parse", "HEAD"))

	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[defaults]\nlanding_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	detected, err := detectProject(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	project, err := registerDetected(ctx, db, detected)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:stack:lead"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	service := testService(home, herdr.NewFake())
	brief := dispatch.Brief{
		Type: "ship", Repos: []string{"worker"},
		BaseByRepo:   map[string]string{"worker": "posse/lower"},
		TargetByRepo: map[string]string{"worker": "staging"},
	}
	members, _, err := service.planTaskMembers(ctx, db, project, cfg, brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].BaseRef != "refs/remotes/origin/posse/lower" || members[0].TargetBranch != "staging" {
		t.Fatalf("stacked member branch plan = %+v", members)
	}
	taskID, _, err := createTaskWithSequence(ctx, db, project, home, store.Task{Type: "ship", Title: "Upper Task", ShortName: "upper-task", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.acquireWorkspaceMount(ctx, db, project, task, members, home, "warm", nil); err != nil {
		t.Fatal(err)
	}
	task, err = db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(task.WorktreePath, "worker")
	if got := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD")); got != lowerSHA {
		t.Fatalf("upper Task started at %s, want lower Task tip %s", got, lowerSHA)
	}
}
