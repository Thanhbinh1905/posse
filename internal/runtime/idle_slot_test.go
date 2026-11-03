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

func TestIdleSlotNoticeCoalescesIssue911Sequence(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "committed"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	for index := 2; index <= 9; index++ {
		queueTask(t, db, project.ID, index)
		if _, err := Run(ctx, db, &herdr.Fake{SnapshotValue: livePane(task)}, project.ID, 20*time.Minute, 3*time.Minute, later.Add(time.Duration(index)*time.Second), nil); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var summary string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(summary) FROM notices WHERE kind='idle_pool_slot'`).Scan(&count, &summary); err != nil || count != 1 {
		t.Fatalf("idle_pool_slot Notice count after eight changing waiting sets = %d, %v; want one", count, err)
	}
	if !strings.Contains(summary, "t9") {
		t.Fatalf("coalesced Notice summary %q does not reflect the latest waiting set", summary)
	}
}

func TestIdleSlotNoticeStartsNewEpisodeAfterConditionEnds(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "committed"); err != nil {
		t.Fatal(err)
	}
	queueTask(t, db, project.ID, 2)
	later := time.Now().Add(time.Hour)
	run := func(at time.Time) {
		t.Helper()
		if _, err := Run(ctx, db, &herdr.Fake{SnapshotValue: livePane(task)}, project.ID, 20*time.Minute, 3*time.Minute, at, nil); err != nil {
			t.Fatal(err)
		}
	}
	run(later)
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='torn-down' WHERE project_id=? AND state='queued'`, project.ID); err != nil {
		t.Fatal(err)
	}
	run(later.Add(time.Second))
	queueTask(t, db, project.ID, 3)
	run(later.Add(2 * time.Second))
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE kind='idle_pool_slot'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("idle_pool_slot Notice count after a new episode = %d, %v; want two", count, err)
	}
}

func TestIdleSlotNoticeNamesPRReleaseCommand(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "committed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET landing_mode='pr',pr_url='https://github.com/acme/repo/pull/1' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	queueTask(t, db, project.ID, 2)
	if _, err := Run(ctx, db, &herdr.Fake{SnapshotValue: livePane(task)}, project.ID, 20*time.Minute, 3*time.Minute, time.Now().Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	var summary string
	if err := db.QueryRowContext(ctx, `SELECT summary FROM notices WHERE kind='idle_pool_slot'`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "posse pr release t1 --policy release") {
		t.Fatalf("Notice summary %q omits the PR release command", summary)
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
