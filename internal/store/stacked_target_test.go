package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTaskTargetChangeIsIdempotentAndRecorded(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	lowerID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Lower", Branch: "posse/lower", TargetBranch: "staging", MergeMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	upperID, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "change", Title: "Upper", Branch: "posse/upper", TargetBranch: "posse/lower", PRURL: "https://github.com/acme/shop/pull/2", MergeMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}

	change, err := db.BeginTaskTargetChange(ctx, project.ID, upperID, "", lowerID, "posse/lower", "staging")
	if err != nil || change.ID == 0 || change.Status != "pending" || change.PRURL == "" {
		t.Fatalf("begin stacked target change = %+v, %v", change, err)
	}
	repeated, err := db.BeginTaskTargetChange(ctx, project.ID, upperID, "", lowerID, "posse/lower", "staging")
	if err != nil || repeated.ID != change.ID {
		t.Fatalf("repeated begin = %+v, %v; want change %d", repeated, err, change.ID)
	}
	if err := db.CompleteTaskTargetChange(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteTaskTargetChange(ctx, change.ID); err != nil {
		t.Fatalf("repeat completion: %v", err)
	}
	upper, err := db.TaskByID(ctx, project.ID, upperID)
	if err != nil || upper.TargetBranch != "staging" {
		t.Fatalf("upper target branch = %q, %v; want staging", upper.TargetBranch, err)
	}
	changes, err := db.TaskTargetChanges(ctx, upperID)
	if err != nil || len(changes) != 1 || changes[0].Status != "complete" || changes[0].FromBranch != "posse/lower" || changes[0].ToBranch != "staging" || changes[0].CompletedAt == 0 {
		t.Fatalf("recorded target changes = %+v, %v", changes, err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, filepath.Dir(db.Path)); err != nil {
		t.Fatalf("rebuild target change from Task snapshots: %v", err)
	}
	upper, err = db.TaskByID(ctx, project.ID, upperID)
	if err != nil || upper.TargetBranch != "staging" {
		t.Fatalf("rebuilt upper target branch = %q, %v; want staging", upper.TargetBranch, err)
	}
	changes, err = db.TaskTargetChanges(ctx, upperID)
	if err != nil || len(changes) != 1 || changes[0].Status != "complete" || changes[0].FromBranch != "posse/lower" || changes[0].ToBranch != "staging" {
		t.Fatalf("rebuilt target change audit = %+v, %v", changes, err)
	}
}

func TestWorkspaceMemberTargetChangeUpdatesOnlyThatMember(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateWorkspaceProject(ctx, "workspace", "/workspace", []ProjectRepo{{Name: "api", Path: "api", DefaultBranch: "main", Status: RepoActive}})
	if err != nil {
		t.Fatal(err)
	}
	lowerID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Lower", Branch: "posse/lower", MergeMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	upperID, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "change", Title: "Upper", Branch: "posse/upper", MergeMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTaskRepos(ctx, lowerID, []TaskRepo{{Repo: "api", TargetBranch: "staging", MergeMode: "pr"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTaskRepos(ctx, upperID, []TaskRepo{{Repo: "api", TargetBranch: "posse/lower", MergeMode: "pr", PRURL: "https://github.com/acme/api/pull/2"}, {Repo: "worker", TargetBranch: "main", MergeMode: "pr"}}); err != nil {
		t.Fatal(err)
	}
	change, err := db.BeginTaskTargetChange(ctx, project.ID, upperID, "api", lowerID, "posse/lower", "staging")
	if err != nil || change.ID == 0 {
		t.Fatalf("begin member target change = %+v, %v", change, err)
	}
	if err := db.CompleteTaskTargetChange(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	repos, err := db.TaskRepos(ctx, upperID)
	if err != nil || len(repos) != 2 || repos[0].TargetBranch != "staging" || repos[1].TargetBranch != "main" {
		t.Fatalf("upper member targets = %+v, %v", repos, err)
	}
}
