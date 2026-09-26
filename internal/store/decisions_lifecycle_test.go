package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAnswerRefusesResolvedSituationEvenBeforeReconciliation(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "A", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateWorking, StateFailed, "worker", "failed"); err != nil {
		t.Fatal(err)
	}
	question := DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "incident-1", Kind: "recovery", Question: "Relaunch?", Options: []string{"relaunch", "discard"}}
	d, err := db.RaiseDecision(ctx, question)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateFailed, StateWorking, "cli", "relaunched"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AnswerDecision(ctx, project.ID, d.ID, "discard", "Discard it"); err == nil {
		t.Fatal("stale Decision accepted")
	}
	d, err = db.Decision(ctx, project.ID, d.ID)
	if err != nil || d.ObsoleteAt == 0 || d.ObsoleteReason == "" || d.Answer != "" {
		t.Fatalf("not obsolete: %#v %v", d, err)
	}
	pending, err := db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stale Decision pending: %#v %v", pending, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 0 {
		t.Fatalf("stale answer Notice: %#v %v", notices, err)
	}
	// The next launch can fail before a reconcile pass sees the working
	// state. Its previous Decision still belongs to the earlier launch.
	if err := db.Transition(ctx, taskID, StateWorking, StateFailed, "worker", "failed again"); err != nil {
		t.Fatal(err)
	}
	current, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "incident-2", Kind: "recovery", Question: "Retry again?", Options: []string{"relaunch", "discard"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunch(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.ObsoleteResolvedDecisions(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	current, err = db.Decision(ctx, project.ID, current.ID)
	if err != nil || current.ObsoleteAt == 0 || current.ObsoleteReason == "" {
		t.Fatalf("previous launch's Decision remained pending: %#v %v", current, err)
	}
}
