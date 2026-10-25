package store

import (
	"context"
	"testing"
)

func TestTaskHistoriesSummarizeTransitions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "A", Profile: "deep", MergeMode: "pr", Branch: "posse/t1", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	histories, err := db.TaskHistories(ctx, project.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := histories[id]
	if got.FirstDoneAt == 0 || got.Landed {
		t.Fatalf("history: %#v", got)
	}
	if future, err := db.TaskHistories(ctx, project.ID, 1<<60); err != nil || len(future) != 0 {
		t.Fatalf("Tasks created before the window leaked in: %#v %v", future, err)
	}
}
