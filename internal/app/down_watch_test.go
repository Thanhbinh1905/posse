package app

import (
	"context"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDownExecuteStopsWatchMaintenanceBeforeTaskTeardown(t *testing.T) {
	fixture := newStartFixture(t)
	t.Setenv("HERDR_PANE_ID", "")
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Queued task", State: store.StateQueued})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	maintenanceStarted := make(chan struct{})
	releaseMaintenance := make(chan struct{})
	fixture.service.downWatchStop = func(context.Context, string, store.Project) error {
		close(maintenanceStarted)
		<-releaseMaintenance
		return nil
	}
	type commandResult struct {
		code   int
		output string
	}
	finished := make(chan commandResult, 1)
	go func() {
		code, output := runCLI(t, fixture.service, "down", "--execute", "--user-approved", "stop project")
		finished <- commandResult{code: code, output: output}
	}()

	<-maintenanceStarted
	db, err = store.OpenReadOnly(fixture.home)
	if err != nil {
		close(releaseMaintenance)
		t.Fatal(err)
	}
	task, err := db.TaskByID(context.Background(), fixture.project.ID, taskID)
	_ = db.Close()
	if err != nil {
		close(releaseMaintenance)
		t.Fatal(err)
	}
	if task.State != store.StateQueued {
		close(releaseMaintenance)
		t.Fatalf("Task state during Watch maintenance = %s, want queued until Watch shutdown completes", task.State)
	}

	close(releaseMaintenance)
	result := <-finished
	if result.code != 0 || !strings.Contains(result.output, "state: down") {
		t.Fatalf("down --execute: exit=%d output=%s", result.code, result.output)
	}
	db, err = store.OpenReadOnly(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	task, err = db.TaskByID(context.Background(), fixture.project.ID, taskID)
	_ = db.Close()
	if err != nil || task.State != store.StateTornDown {
		t.Fatalf("Task after down --execute = %s, err=%v; want torn-down", task.State, err)
	}
}
