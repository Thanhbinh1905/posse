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

func TestWorkspaceTaskFreezesMemberTargetAndStartsFromBriefBase(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceFixture(t, root)
	workerRoot := filepath.Join(workspace, "worker")
	gitTest(t, workerRoot, "branch", "staging", "main")
	gitTest(t, workerRoot, "push", "origin", "staging")

	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configText := `[defaults]
landing_mode = "local"
target_branch = "staging"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
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
	brief := dispatch.Brief{Type: "ship", Repos: []string{"worker"}, BaseByRepo: map[string]string{"worker": "staging"}}
	members, _, err := service.planTaskMembers(ctx, db, project, cfg, brief, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].TargetBranch != "staging" || members[0].BaseRef != "refs/remotes/origin/staging" {
		t.Fatalf("workspace branch plan = %+v", members)
	}
	taskID, _, err := createTaskWithSequence(ctx, db, project, home, store.Task{Type: "ship", Title: "Stack on staging", ShortName: "stack-staging", LandingMode: "local"})
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
	taskRepos, err := db.TaskRepos(ctx, task.ID)
	if err != nil || len(taskRepos) != 1 || taskRepos[0].TargetBranch != "staging" || taskRepos[0].BaseRef != "refs/remotes/origin/staging" {
		t.Fatalf("persisted workspace branch plan = %+v, %v", taskRepos, err)
	}
	worktree := filepath.Join(task.WorktreePath, "worker")
	if got := strings.TrimSpace(gitTest(t, worktree, "rev-parse", "HEAD")); got != strings.TrimSpace(gitTest(t, workerRoot, "rev-parse", "refs/heads/staging")) {
		t.Fatalf("member Task started at %s, want staging tip", got)
	}
}
