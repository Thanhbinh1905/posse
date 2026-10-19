package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestOpenGitLabMRWatchDoesNotRejectAChangedTarget(t *testing.T) {
	t.Setenv("POSSE_TEST_GLAB_BASE_BRANCH", "staging")
	fixture := gitlabFixture(t, store.StateWorking)
	fixture.setGitLabState(t, "opened", "mergeable", "running", fixture.headSHA)
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	forge, err := forgeForRepository(context.Background(), fixture.repo, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	task := fixture.task
	task.PRURL = "https://git.example.com/group/sub/shop/-/merge_requests/17"
	observation, _, err := gitlabObservationWithChecks(context.Background(), forge, task, "main", time.Now().UnixMilli(), false)
	if err != nil || observation.State != "OPEN" || decodeChecks(observation.Checks).State != "PENDING" {
		t.Fatalf("open MR with an external retarget was not observed without accepting CI: %+v, %v", observation, err)
	}
	fixture.setGitLabState(t, "merged", "mergeable", "success", fixture.headSHA)
	if _, _, err := gitlabObservation(context.Background(), forge, task, "main", time.Now().UnixMilli()); err == nil || !strings.Contains(err.Error(), `targets "staging", expected "main"`) {
		t.Fatalf("merged MR with a non-frozen target error = %v", err)
	}
}

func TestMergedLowerTaskRetargetsUpperGitLabMR(t *testing.T) {
	fixture := gitlabFixture(t, store.StateWorking)
	ctx := context.Background()
	lower := fixture.task
	lower.TargetBranch = "staging"
	const upperBranch = "posse/t2"
	const upperURL = "https://git.example.com/group/sub/shop/-/merge_requests/18"
	upperID, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
		Seq: 2, Type: "change", Title: "Upper Task", State: store.StateSpawning,
		MergeMode: "pr", Branch: upperBranch, BaseRef: "refs/remotes/origin/posse/t1",
		TargetBranch: lower.Branch, PRURL: upperURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, upperID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{
		"web_url": upperURL, "state": "opened", "sha": fixture.headSHA,
		"source_branch": upperBranch, "target_branch": lower.Branch,
		"source_project_id": 7, "target_project_id": 7,
		"detailed_merge_status": "mergeable", "merge_commit_sha": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.ghState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.enqueueStackedTargetChanges(ctx, fixture.db, fixture.project, lower, "", lower.TargetBranch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.processPendingStackedTargetChanges(ctx, fixture.db, fixture.project, cfg, ""); err != nil {
		t.Fatal(err)
	}
	upper, err := fixture.db.TaskByID(ctx, fixture.project.ID, upperID)
	if err != nil || upper.TargetBranch != lower.TargetBranch {
		t.Fatalf("upper stored target = %q, %v; want %q", upper.TargetBranch, err, lower.TargetBranch)
	}
	var updated map[string]any
	data, err := os.ReadFile(fixture.ghState)
	if err != nil || json.Unmarshal(data, &updated) != nil || updated["target_branch"] != lower.TargetBranch {
		t.Fatalf("upper MR state = %s, %v; want target %q", data, err, lower.TargetBranch)
	}
	changes, err := fixture.db.TaskTargetChanges(ctx, upperID)
	if err != nil || len(changes) != 1 || changes[0].Status != "complete" {
		t.Fatalf("target change audit = %+v, %v", changes, err)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(log), "mr update 18 --repo https://git.example.com/group/sub/shop --target-branch staging"); count != 1 {
		t.Fatalf("GitLab MR update count = %d; glab log: %s", count, log)
	}
}

func TestWorkspaceStackedRetargetChangesOnlyTheMergedMember(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	ctx := context.Background()
	workspaceRoot := filepath.Join(fixture.root, "workspace")
	apiRoot := fixture.repo
	workerRoot := filepath.Join(fixture.root, "worker")
	if err := os.MkdirAll(workerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := fixture.db.CreateWorkspaceProject(ctx, "workspace", workspaceRoot, []store.ProjectRepo{
		{Name: "api", Path: "api", DefaultBranch: "main", Status: store.RepoActive},
		{Name: "worker", Path: "worker", DefaultBranch: "main", Status: store.RepoActive},
	})
	if err != nil {
		t.Fatal(err)
	}
	lowerID, err := fixture.db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Lower", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/lower"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, lowerID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	upperID, err := fixture.db.CreateTask(ctx, project.ID, store.Task{Seq: 2, Type: "change", Title: "Upper", State: store.StateSpawning, MergeMode: "pr", Branch: "posse/upper"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, upperID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.CreateTaskRepos(ctx, lowerID, []store.TaskRepo{{Repo: "api", WorktreePath: apiRoot, TargetBranch: "staging", MergeMode: "pr"}}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.CreateTaskRepos(ctx, upperID, []store.TaskRepo{
		{Repo: "api", WorktreePath: apiRoot, TargetBranch: "posse/lower", MergeMode: "pr"},
		{Repo: "worker", WorktreePath: workerRoot, TargetBranch: "main", MergeMode: "pr"},
	}); err != nil {
		t.Fatal(err)
	}
	lower, err := fixture.db.TaskByID(ctx, project.ID, lowerID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.enqueueStackedTargetChanges(ctx, fixture.db, project, lower, "api", "staging"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.processPendingStackedTargetChanges(ctx, fixture.db, project, cfg, "api"); err != nil {
		t.Fatal(err)
	}
	upperRepos, err := fixture.db.TaskRepos(ctx, upperID)
	if err != nil || len(upperRepos) != 2 || upperRepos[0].TargetBranch != "staging" || upperRepos[1].TargetBranch != "main" {
		t.Fatalf("upper member targets = %+v, %v", upperRepos, err)
	}
	changes, err := fixture.db.TaskTargetChanges(ctx, upperID)
	if err != nil || len(changes) != 1 || changes[0].Repo != "api" || changes[0].Status != "complete" {
		t.Fatalf("workspace target change audit = %+v, %v", changes, err)
	}
}
