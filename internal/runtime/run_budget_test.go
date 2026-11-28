package runtime

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func budgetRunFixture(t *testing.T, taskCount int) (*store.DB, store.Project, *herdr.Fake) {
	t.Helper()
	db, project, first := createWorkingTask(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	fake := herdr.NewFake()
	fake.SnapshotValue.ServerStartedAt = "budget-generation"
	for seq := 1; seq <= taskCount; seq++ {
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
	return db, project, fake
}

func TestRunDefersRemainingTasksWhenBudgetSpent(t *testing.T) {
	db, project, fake := budgetRunFixture(t, 32)
	ctx := context.Background()
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
	// The deadline limits new work, not when in-flight writes return under load.
	for pass := 0; pass < 4; pass++ {
		if _, err := Run(ctx, db, fake, project.ID, 0, 0, time.Now(), nil); err != nil && !store.IsOnlyBusy(err) {
			t.Fatal(err)
		}
		var updatedAt int64
		if err := db.QueryRowContext(ctx, `SELECT updated_at FROM tasks WHERE project_id=? AND seq=32`, project.ID).Scan(&updatedAt); err != nil {
			t.Fatal(err)
		}
		if updatedAt > 1 {
			return
		}
	}
	t.Fatal("deferred observations did not progress across four bounded passes")
}

type budgetSnapshot struct {
	*herdr.Fake
	delay time.Duration
}

func (s budgetSnapshot) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	time.Sleep(s.delay)
	return s.Fake.Snapshot(ctx)
}

func TestRunRetryDefersRemainingTasksWhenBudgetSpent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db, project, fake := budgetRunFixture(t, 2)
		ctx := t.Context()
		assertObserved := func(want ...int) {
			t.Helper()
			tasks, err := db.LiveTasks(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			var observed []int
			for _, task := range tasks {
				if task.AgentServerStartedAt == fake.SnapshotValue.ServerStartedAt {
					observed = append(observed, task.Seq)
				}
			}
			if !slices.Equal(observed, want) {
				t.Fatalf("persisted observations for Tasks %v, want %v", observed, want)
			}
		}

		const remainingBudget = store.ObservationWriteBusyTimeout / 2
		slowSnapshot := budgetSnapshot{Fake: fake, delay: ReconcileBudget - remainingBudget}
		if _, err := Run(ctx, db, slowSnapshot, project.ID, 0, 0, time.Now(), nil); !store.IsOnlyBusy(err) {
			t.Fatalf("initial pass error = %v, want typed deferral", err)
		}
		assertObserved()

		retrySnapshot := budgetSnapshot{Fake: fake, delay: ReconcileBudget / 2}
		var admitted []int
		recordUsage := func(_ context.Context, task store.Task) error {
			admitted = append(admitted, task.Seq)
			if task.Seq == 1 {
				time.Sleep(ReconcileBudget - retrySnapshot.delay - remainingBudget)
			}
			return nil
		}
		_, err := Run(ctx, db, retrySnapshot, project.ID, 0, 0, time.Now(), nil, recordUsage)
		if !slices.Equal(admitted, []int{1}) {
			t.Fatalf("retry admitted Tasks %v, want only t1 before deferring; Run error: %v", admitted, err)
		}
		if !store.IsOnlyBusy(err) {
			t.Fatalf("retry error = %v, want typed deferral", err)
		}
		assertObserved(1)

		if _, err := Run(ctx, db, fake, project.ID, 0, 0, time.Now(), nil); err != nil {
			t.Fatalf("next pass error = %v, want nil", err)
		}
		assertObserved(1, 2)
	})
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
