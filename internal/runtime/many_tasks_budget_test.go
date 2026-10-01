package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReviewRunBudgetWithManyContendedTasks(t *testing.T) {
	db, project, first := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	panes := []herdr.Pane{{PaneID: first.PaneID, WorkspaceID: first.HerdrWorkspaceID, Label: first.PaneLabel, Agent: "claude", AgentStatus: "working"}}
	for seq := 2; seq <= 16; seq++ {
		paneID := fmt.Sprintf("w1:p%d", seq)
		label := fmt.Sprintf("posse:shop:t%d", seq)
		id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: seq, Type: "ship", Title: fmt.Sprintf("Task %d", seq), LandingMode: "local", WorktreePath: filepath.Join(t.TempDir(), fmt.Sprintf("task-%d", seq)), PaneID: paneID, PaneLabel: label, HerdrWorkspaceID: "w1", AutonomyReview: "ask", AutonomyLand: "ask"})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "agent ready"); err != nil {
			t.Fatal(err)
		}
		panes = append(panes, herdr.Pane{PaneID: paneID, WorkspaceID: "w1", Label: label, Agent: "claude", AgentStatus: "working"})
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	adapter := herdr.NewFake()
	adapter.SnapshotValue = herdr.Snapshot{Panes: panes}
	started := time.Now()
	_, runErr := Run(ctx, db, adapter, project.ID, 0, 0, started, nil)
	elapsed := time.Since(started)
	t.Logf("runtime.Run with 16 Tasks and held SQLite writer took %s, error=%v", elapsed, runErr)
	if !store.IsBusy(runErr) {
		t.Fatalf("Run error = %v, want typed contention", runErr)
	}
	if elapsed > ReconcileBudget+250*time.Millisecond {
		t.Fatalf("one Run exceeded its total %s budget: elapsed=%s", ReconcileBudget, elapsed)
	}
}

func TestRunPrioritizesDeferredTaskObservations(t *testing.T) {
	db, project, first := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	adapter := herdr.NewFake()
	for seq := 1; seq <= 4; seq++ {
		task := first
		if seq > 1 {
			task.ID = 0
			task.Seq = seq
			task.State = store.StateSpawning
			task.PaneID = fmt.Sprintf("w1:p%d", seq)
			task.PaneLabel = fmt.Sprintf("posse:shop:t%d", seq)
			task.WorktreePath = t.TempDir()
			id, err := db.CreateTask(ctx, project.ID, task)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "agent ready"); err != nil {
				t.Fatal(err)
			}
		}
		adapter.SnapshotValue.Panes = append(adapter.SnapshotValue.Panes, herdr.Pane{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"})
	}
	// Model a prior pass that observed t1/t2 but deferred the older t3/t4.
	// Audit actual observation writes, not the implementation's sorted slice.
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET updated_at=CASE WHEN seq<=2 THEN 1000+seq ELSE 100-seq END WHERE project_id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE observation_order(seq INTEGER NOT NULL)`,
		`CREATE TRIGGER audit_observation AFTER UPDATE OF pane_id ON tasks BEGIN INSERT INTO observation_order(seq) VALUES(NEW.seq); END`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	observe := func() []int {
		t.Helper()
		if _, err := Run(ctx, db, adapter, project.ID, 0, 0, time.Now(), nil); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(ctx, `SELECT seq FROM observation_order ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var order []int
		for rows.Next() {
			var seq int
			if err := rows.Scan(&seq); err != nil {
				t.Fatal(err)
			}
			order = append(order, seq)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return order
	}
	if order := observe(); !slices.Equal(order, []int{4, 3, 1, 2}) {
		t.Fatalf("observation order = %v, want deferred oldest Tasks first: [4 3 1 2]", order)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM observation_order`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET updated_at=1 WHERE project_id=? AND seq=2`, project.ID); err != nil {
		t.Fatal(err)
	}
	if order := observe(); len(order) != 4 || order[0] != 2 {
		t.Fatalf("next-pass observation order = %v, want newly deferred t2 first", order)
	}
}
