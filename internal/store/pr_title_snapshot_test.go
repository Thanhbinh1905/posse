package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTaskPRTitleSurvivesSnapshotRebuild(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Brief title", ShortName: "brief-title", MergeMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	want := "fix(publish): keep the retitled PR"
	if err := db.SetTaskPRTitle(ctx, taskID, want); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET pr_title='' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil || rebuilt.Title != "Brief title" || rebuilt.PRTitle != want {
		t.Fatalf("rebuilt Task = %#v, %v", rebuilt, err)
	}
}
