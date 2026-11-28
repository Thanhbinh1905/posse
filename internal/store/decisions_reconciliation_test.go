package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestDecisionsReconcileAfterTaskTeardown(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "A", MergeMode: "local", State: StateQueued})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateQueued, StateTornDown, "cli", "released"); err != nil {
		t.Fatal(err)
	}
	d, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "late-question", Kind: "worker_question", Question: "Continue?", Options: []string{"yes", "no"}})
	if err != nil {
		t.Fatal(err)
	}
	review, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "late-review", Kind: "review", Question: "Handle findings?", Options: []string{"accept", "ignore"}})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.Decisions(ctx, project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("torn-down Task Decision remains pending: %#v", pending)
	}
	for _, id := range []int64{d.ID, review.ID} {
		decision, err := db.Decision(ctx, project.ID, id)
		if err != nil || decision.ObsoleteAt == 0 {
			t.Fatalf("Decision %d not obsoleted: %#v %v", id, decision, err)
		}
	}
}

func TestUnrecoverableOrphanDecisionIsObsoletedAfterTeardown(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "A", MergeMode: "local", State: StateQueued})
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "orphan:unrecoverable:1", Kind: "orphan", Question: "Repair or discard?", Options: []string{"repair", "discard"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateQueued, StateTornDown, "cli", "released"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.Decisions(ctx, project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("unrecoverable Orphan Decision remains pending after teardown: %#v", pending)
	}
	d, err = db.Decision(ctx, project.ID, d.ID)
	if err != nil || d.ObsoleteAt == 0 {
		t.Fatalf("Orphan Decision not obsoleted: %#v %v", d, err)
	}
}

func TestTeardownDependantsDecisionObsoletedWhenNoneRemainQueued(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "A", MergeMode: "local", State: StateQueued})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := db.CreateTask(ctx, project.ID, Task{Seq: 2, Type: "change", Title: "B", MergeMode: "local", State: StateQueued})
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: parent, Origin: "teardown-dependants:t1:t2", Kind: "teardown_dependants", Question: "Re-point?", Options: []string{"re-point", "cascade"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, parent, StateQueued, StateTornDown, "cli", "released"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 1 || pending[0].ID != d.ID {
		t.Fatalf("Decision disappeared while a named dependant remained queued: %#v %v", pending, err)
	}
	if err := db.Transition(ctx, dependent, StateQueued, StateTornDown, "cli", "released"); err != nil {
		t.Fatal(err)
	}
	if err := db.ObsoleteResolvedDecisions(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	d, err = db.Decision(ctx, project.ID, d.ID)
	if err != nil || d.ObsoleteAt == 0 {
		t.Fatalf("Decision not obsoleted: %#v %v", d, err)
	}
}
