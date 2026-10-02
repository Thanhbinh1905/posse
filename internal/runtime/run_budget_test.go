package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRunDefersRemainingTasksWhenBudgetSpent(t *testing.T) {
	db, project, first := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	fake := herdr.NewFake()
	fake.SnapshotValue.ServerStartedAt = "budget-generation"
	for seq := 1; seq <= 32; seq++ {
		task := first
		if seq > 1 {
			task.Seq = seq
			task.ID = 0
			task.State = store.StateSpawning
			task.PaneID = fmt.Sprintf("w1:p%d", seq)
			task.PaneLabel = fmt.Sprintf("posse:shop:t%d", seq)
			id, err := db.CreateTask(ctx, project.ID, task)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
				t.Fatal(err)
			}
		}
		fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"})
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET updated_at=1 WHERE project_id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	started := time.Now()
	_, err = Run(ctx, db, fake, project.ID, 0, 0, time.Now(), nil)
	t.Logf("contended Run took %s: %v", time.Since(started), err)
	if !store.IsBusy(err) {
		t.Fatalf("Run error = %v, want typed contention", err)
	}
	if elapsed := time.Since(started); elapsed > ReconcileBudget+250*time.Millisecond {
		t.Fatalf("Run exceeded its budget: %s > %s", elapsed, ReconcileBudget)
	}
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, db, fake, project.ID, 0, 0, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	var updatedAt int64
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM tasks WHERE project_id=? AND seq=32`, project.ID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt <= 1 {
		t.Fatal("deferred observations did not progress on the next pass")
	}
}

func TestRunBoundsMissingPaneTransition(t *testing.T) {
	db, project, _ := createWorkingTask(t)
	defer db.Close()
	writer, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	started := time.Now()
	_, err = Run(context.Background(), db, herdr.NewFake(), project.ID, 0, 0, time.Now(), nil)
	if !store.IsBusy(err) {
		t.Fatalf("Run error = %v, want typed contention", err)
	}
	if elapsed := time.Since(started); elapsed > ReconcileBudget+250*time.Millisecond {
		t.Fatalf("missing-pane transition outlived Run budget: %s", elapsed)
	}
}
