package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func queueTask(t *testing.T, db *store.DB, projectID int64, seq int) {
	t.Helper()
	task := store.Task{Seq: seq, Type: "change", Title: "Waiting work", State: store.StateQueued, MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"}
	if _, err := db.CreateTask(context.Background(), projectID, task); err != nil {
		t.Fatal(err)
	}
}

func TestIdleSlotNoticeNamesIdleAndWaitingTasks(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "committed"); err != nil {
		t.Fatal(err)
	}
	queueTask(t, db, project.ID, 2)
	later := time.Now().Add(time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := Run(ctx, db, &herdr.Fake{SnapshotValue: livePane(task)}, project.ID, 20*time.Minute, 3*time.Minute, later.Add(time.Duration(i)*time.Second), nil); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var summary string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(summary),'') FROM notices WHERE kind='idle_pool_slot'`).Scan(&count, &summary); err != nil || count != 1 {
		t.Fatalf("idle_pool_slot Notice count = %d, %v", count, err)
	}
	if !strings.Contains(summary, "t1") || !strings.Contains(summary, "t2") {
		t.Fatalf("Notice summary %q does not name t1 and t2", summary)
	}
}

func TestIdleSlotNoticeNeedsWaitingTaskAndQuietHolder(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "committed"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	run := func(at time.Time) {
		t.Helper()
		if _, err := Run(ctx, db, &herdr.Fake{SnapshotValue: livePane(task)}, project.ID, 20*time.Minute, 3*time.Minute, at, nil); err != nil {
			t.Fatal(err)
		}
	}
	run(later)
	queueTask(t, db, project.ID, 2)
	run(time.Now().Add(time.Minute))
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE kind='idle_pool_slot'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("Notice raised with no waiting Task or a recent holder: count=%d, %v", count, err)
	}
}

func livePane(task store.Task) herdr.Snapshot {
	return herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "idle"}}}
}
