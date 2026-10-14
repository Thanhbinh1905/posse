package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestResumedRiderCannotLandOrRaiseLandReady(t *testing.T) {
	f := newPRLandingFixture(t, "pr", store.StateDone)
	ctx := context.Background()
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetTaskGatedSHA(ctx, f.task.ID, f.headSHA); err != nil {
		t.Fatal(err)
	}
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: f.task.PaneID, Label: f.task.PaneLabel, Agent: "pi", AgentStatus: "working"}}}
	// Fixture tasks without a pane still adopt their labelled Rider.
	if snapshot.Panes[0].PaneID == "" {
		snapshot.Panes[0].PaneID = "w1:p2"
		if err := f.db.UpdateTaskLaunch(ctx, f.task.ID, f.worktree, "w1", "w1:p2", "rider-t2", "rider"); err != nil {
			t.Fatal(err)
		}
		snapshot.Panes[0].Label = "rider-t2"
	}
	now := time.Now()
	for _, when := range []time.Time{now, now.Add(runtime.CompletedRiderBusyGrace)} {
		if _, err := runtime.ReconcileSnapshot(ctx, f.db, f.project.ID, snapshot, when); err != nil {
			t.Fatal(err)
		}
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateWorking {
		t.Fatalf("Rider not reopened: %#v %v", task, err)
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
		if notice.Kind == "land_ready" {
			t.Fatalf("working Rider raised land_ready: %#v", notice)
		}
	}
	if code, stdout, stderr := f.run("land", "t1"); code == 0 || !strings.Contains(stdout+stderr, "done or landing") {
		t.Fatalf("Land accepted resumed Rider: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

func TestPRReadinessRequiresCompletedTask(t *testing.T) {
	for _, state := range []store.State{store.StateWorking, store.StateBlocked, store.StateStalled, store.StateNeedsDecision, store.StateFailed, store.StateLost, store.StateDone, store.StateLanding} {
		task := store.Task{State: state}
		observed := store.PRObservation{State: "OPEN", Checks: `{"state":"SUCCESS"}`, Review: "APPROVED", Mergeable: "MERGEABLE"}
		effect := prObservationEffect(store.Project{}, task, observed, nil, store.PRObservation{}, false)
		ready := false
		for _, notice := range effect.Notices {
			if notice.Kind == "land_ready" {
				ready = true
			}
		}
		if ready != (state == store.StateDone || state == store.StateLanding) {
			t.Errorf("state %s: land_ready=%v", state, ready)
		}
	}
}
