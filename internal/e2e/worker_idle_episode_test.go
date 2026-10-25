//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A Worker that goes idle, resumes and goes idle again within one launch
// raises one worker_idle Notice per idle episode, none repeated inside one.
func TestWorkerIdleNoticeIsRaisedOncePerIdleEpisode(t *testing.T) {
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()
	ctx := context.Background()
	const workspaceID, workerPane = "w2", "w2:p1"
	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Idle episodes", MergeMode: "local", PaneID: workerPane,
		PaneLabel: "posse:shop:t1", HerdrWorkspaceID: workspaceID, WorktreePath: f.repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	snapshot := func(status string) herdr.Snapshot {
		return herdr.Snapshot{Panes: []herdr.Pane{{PaneID: workerPane, WorkspaceID: workspaceID, Label: "posse:shop:t1", Agent: "claude", AgentStatus: status}}}
	}
	idleNotices := func() int {
		notices, err := f.db.Notices(ctx, f.project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, notice := range notices {
			if notice.Kind == "worker_idle" {
				count++
			}
		}
		return count
	}
	now := time.Now().Truncate(time.Millisecond)
	reconcile := func(status string, at time.Duration) {
		t.Helper()
		if _, err := runtime.ReconcileSnapshot(ctx, f.db, f.project.ID, snapshot(status), now.Add(at), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute} {
		reconcile("idle", at)
	}
	if got := idleNotices(); got != 1 {
		t.Fatalf("first idle episode raised %d worker_idle Notices, want 1", got)
	}
	reconcile("working", 4*time.Minute)
	for _, at := range []time.Duration{5 * time.Minute, 6 * time.Minute, 7 * time.Minute, 8 * time.Minute} {
		reconcile("idle", at)
	}
	if got := idleNotices(); got != 2 {
		t.Fatalf("after a second idle episode in the same launch there are %d worker_idle Notices, want 2", got)
	}
}
