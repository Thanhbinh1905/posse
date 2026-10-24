package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestResumedWorkerCannotMergeOrRaiseMergeReady(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := f.db.UpdateTaskMerge(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTaskGatedSHA(ctx, f.task.ID, f.headSHA); err != nil {
		t.Fatal(err)
	}
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: f.task.PaneID, Label: f.task.PaneLabel, Agent: "pi", AgentStatus: "working"}}}
	// Fixture tasks without a pane still adopt their labelled Worker.
	if snapshot.Panes[0].PaneID == "" {
		snapshot.Panes[0].PaneID = "w1:p2"
		if err := f.db.UpdateTaskLaunch(ctx, f.task.ID, f.worktree, "w1", "w1:p2", "worker-t2", "worker"); err != nil {
			t.Fatal(err)
		}
		snapshot.Panes[0].Label = "worker-t2"
	}
	now := time.Now()
	snapshot.Panes[0].AgentStatus = "idle"
	if _, err := runtime.ReconcileSnapshot(ctx, f.db, f.project.ID, snapshot, now); err != nil {
		t.Fatal(err)
	}
	// The new turn changes the Task repository, so it reopens the Task.
	if err := os.WriteFile(filepath.Join(f.task.WorktreePath, "new-turn.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot.Panes[0].AgentStatus = "working"
	if _, err := runtime.ReconcileSnapshot(ctx, f.db, f.project.ID, snapshot, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateWorking {
		t.Fatalf("Worker not reopened: %#v %v", task, err)
	}
	observation := store.PRObservation{ProjectID: f.project.ID, TaskID: task.ID, PRURL: task.PRURL, HeadSHA: f.headSHA, State: "OPEN", Checks: `{"state":"SUCCESS"}`, Review: "APPROVED", Mergeable: "MERGEABLE", ObservedAt: now.UnixMilli()}
	effect := prObservationEffect(f.project, task, observation, nil, store.PRObservation{}, false)
	if _, err := f.db.RecordPRObservation(ctx, observation, effect, task); err != nil {
		t.Fatal(err)
	}
	if err := f.service.raiseNoticeDecisions(ctx, f.db, f.project); err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "merge_ready" {
			t.Fatalf("working Worker raised merge_ready: %#v", notice)
		}
	}
	if code, stdout, stderr := f.run("merge", "t1"); code == 0 || !strings.Contains(stdout+stderr, "done or merging") {
		t.Fatalf("Merge accepted resumed Worker: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

func TestPRReadinessRequiresCompletedTask(t *testing.T) {
	for _, state := range []store.State{store.StateWorking, store.StateBlocked, store.StateStalled, store.StateNeedsDecision, store.StateFailed, store.StateLost, store.StateDone, store.StateMerging} {
		task := store.Task{State: state}
		observed := store.PRObservation{State: "OPEN", Checks: `{"state":"SUCCESS"}`, Review: "APPROVED", Mergeable: "MERGEABLE"}
		effect := prObservationEffect(store.Project{}, task, observed, nil, store.PRObservation{}, false)
		ready := false
		for _, notice := range effect.Notices {
			if notice.Kind == "merge_ready" {
				ready = true
			}
		}
		if ready != (state == store.StateDone || state == store.StateMerging) {
			t.Errorf("state %s: merge_ready=%v", state, ready)
		}
	}
}
