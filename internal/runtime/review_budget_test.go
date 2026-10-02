package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Regression from t152 at the complete Run seam, not ReconcileSnapshot.
func TestReviewRunHonorsReconcileBudgetUnderGenerationWriteContention(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	writer, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		ServerStartedAt: "review-generation",
		Panes:           []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}},
	}
	started := time.Now()
	_, err = Run(context.Background(), db, fake, project.ID, 0, 0, time.Now(), nil)
	elapsed := time.Since(started)
	t.Logf("Run elapsed=%s budget=%s busy=%t deadline=%t error=%v", elapsed, ReconcileBudget, store.IsBusy(err), errors.Is(err, context.DeadlineExceeded), err)
	if elapsed > ReconcileBudget+250*time.Millisecond {
		t.Fatalf("Run outlived its bounded reconciliation context: %s > %s", elapsed, ReconcileBudget)
	}
}
