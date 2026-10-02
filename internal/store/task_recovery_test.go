package store

import (
	"context"
	"sync"
	"testing"
)

func TestTaskRecoveryBudgetBackoffAndExhaustionSurviveReopen(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Broken Rider"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	claim := func(generation string, expected, owner int, now int64, want bool) {
		t.Helper()
		got, err := db.ClaimTaskRecovery(ctx, id, generation, expected, owner, 2, now, now+100)
		if err != nil || got != want {
			t.Fatalf("claim generation=%s expected=%d now=%d: %v, %v; want %v", generation, expected, now, got, err, want)
		}
	}
	claim("server-1", 0, 101, 1000, true)
	claim("server-2", 0, 102, 1200, false) // Live owner cannot be replaced.
	if err := db.FinishTaskRecovery(ctx, task, 101, 2, false, "prompt-wait", 2000); err != nil {
		t.Fatal(err)
	}
	claim("server-2", 0, 102, 1999, false) // Changing generation cannot bypass backoff.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	claim("server-2", 0, 102, 2000, true)
	state, err := db.TaskRecovery(ctx, id)
	if err != nil || state.Attempts != 2 {
		t.Fatalf("budget reset on generation/reopen: %#v %v", state, err)
	}
	// Final attempt crashes. Its next recovery records exhaustion instead of
	// starting another process. Repeating finalization still raises one Notice.
	if err := db.FinishTaskRecovery(ctx, task, 102, 2, false, "process exited", 3000); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishTaskRecovery(ctx, task, 0, 2, false, "late event", 4000); err != nil {
		t.Fatal(err)
	}
	claim("server-3", 0, 103, 5000, false)
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "recovery_failed" {
		t.Fatalf("exhaustion Notices=%#v err=%v", notices, err)
	}
	// Explicit successful relaunch resets this episode while keeping the
	// current generation settled. A later real restart receives a new budget.
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET agent_server_started_at='server-3' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.ResetTaskRecovery(ctx, id); err != nil {
		t.Fatal(err)
	}
	claim("server-3", 0, 104, 6000, false)
	claim("server-4", 0, 104, 6000, true)
	state, err = db.TaskRecovery(ctx, id)
	if err != nil || state.Attempts != 1 {
		t.Fatalf("new episode=%#v err=%v", state, err)
	}
}

func TestConcurrentTaskRecoveryClaimsChargeOneAttempt(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Rider"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 16)
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			claimed, err := db.ClaimTaskRecovery(ctx, id, "server-1", 0, 100, 3, 1000, 2000)
			results <- claimed
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	count := 0
	for claimed := range results {
		if claimed {
			count++
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := db.TaskRecovery(ctx, id)
	if err != nil || count != 1 || state.Attempts != 1 {
		t.Fatalf("concurrent claims=%d state=%#v err=%v", count, state, err)
	}
	// Takeover of the expected dead owner also consumes an attempt.
	claimed, err := db.ClaimTaskRecovery(ctx, id, "server-1", 100, 101, 3, 2000, 3000)
	state, readErr := db.TaskRecovery(ctx, id)
	if err != nil || readErr != nil || !claimed || state.Attempts != 2 {
		t.Fatalf("dead owner takeover=%v state=%#v err=%v readErr=%v", claimed, state, err, readErr)
	}
}
