package store

import (
	"errors"
	"strings"
	"testing"
)

func TestWorkerDependencySurvivesSnapshotRebuildAndLandedTeardown(t *testing.T) {
	ctx := t.Context()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	workerID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Worker"})
	if err != nil {
		t.Fatal(err)
	}
	dependencyID, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "change", Title: "Dependency"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{workerID, dependencyID} {
		if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "Worker started"); err != nil {
			t.Fatal(err)
		}
	}
	worker, err := db.TaskByID(ctx, project.ID, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordWorkerSignal(ctx, worker, "blocked", "Waiting for the fix", map[string]any{"on": "t2"}, "task_dependency_wait"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatal(err)
	}
	if wait, err := db.TaskDependencyWait(ctx, workerID); err != nil || !strings.HasPrefix(wait, "t2:landed") {
		t.Fatalf("rebuilt wait = %q, err = %v", wait, err)
	}
	if err := db.Transition(ctx, workerID, StateBlocked, StateWorking, "herdr", "idle pane"); !errors.Is(err, ErrStateRace) {
		t.Fatalf("Herdr cleared a dependency wait: %v", err)
	}
	if err := db.UpdateTaskMerge(ctx, dependencyID, "", "landed-head"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, dependencyID, StateWorking, StateMerged, "cli", "Landed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, dependencyID, StateMerged, StateTornDown, "cli", "Released Sandbox"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := db.ReconcileDependencyWait(ctx, workerID); err != nil {
			t.Fatal(err)
		}
	}
	worker, err = db.TaskByID(ctx, project.ID, workerID)
	if err != nil || worker.State != StateWorking {
		t.Fatalf("worker state = %s, err = %v", worker.State, err)
	}
	messages, err := db.PendingTaskInstructions(ctx, workerID)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "Dependency t2 landed") {
		t.Fatalf("resume instructions = %+v, err = %v", messages, err)
	}
	if _, err := db.RecordWorkerSignal(ctx, worker, "done", "Premature done", nil, "task_done"); !errors.Is(err, ErrQueuedInstructionsPending) {
		t.Fatalf("done bypassed the queued resume instruction: %v", err)
	}
}
