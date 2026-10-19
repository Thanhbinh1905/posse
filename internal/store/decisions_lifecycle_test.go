package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeReadyDecisionIsObsoletedWhenLatestPRObservationIsStale(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "A", MergeMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to State
		source   string
	}{{StateSpawning, StateWorking, "cli"}, {StateWorking, StateDone, "worker"}, {StateDone, StateMerging, "cli"}} {
		if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	const prURL, head = "https://github.com/acme/shop/pull/1", "head-sha"
	if err := db.UpdateTaskMerge(ctx, taskID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTaskGatedSHA(ctx, taskID, head); err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	observation := PRObservation{ProjectID: project.ID, TaskID: taskID, PRURL: prURL, HeadSHA: head, State: "OPEN", Checks: `{"state":"SUCCESS"}`, Review: "APPROVED", Mergeable: "MERGEABLE", ObservedAt: 1}
	if _, err := db.RecordPRObservation(ctx, observation, PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	decision, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "pr_ready:" + prURL, Kind: "merge_ready", Question: "Merge this PR?", Options: []string{"merge", "wait"}})
	if err != nil {
		t.Fatal(err)
	}
	observation.Checks = `{"state":"STALE"}`
	observation.ObservedAt++
	if _, err := db.RecordPRObservation(ctx, observation, PRObservationEffect{}, task); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AnswerDecision(ctx, project.ID, decision.ID, "merge", "User approved merge"); err == nil || !strings.Contains(err.Error(), "PR is no longer ready to merge") {
		t.Fatalf("stale PR Decision accepted or had the wrong reason: %v", err)
	}
	pending, err := db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 0 {
		t.Fatalf("stale PR left a merge Decision pending: %#v %v", pending, err)
	}
	decision, err = db.Decision(ctx, project.ID, decision.ID)
	if err != nil || decision.ObsoleteAt == 0 || decision.ObsoleteReason != "PR is no longer ready to merge" {
		t.Fatalf("stale PR Decision = %#v, %v", decision, err)
	}
}

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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "A", MergeMode: "local"})
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
