package app

import (
	"context"
	"testing"
)

func TestAnsweredInvalidPRDecisionIsNotReofferedOnEveryPoll(t *testing.T) {
	f := newPRLandingFixture(t, "pr", "done")
	defer f.db.Close()
	ctx := context.Background()
	if err := f.db.UpdateTaskLanding(ctx, f.task.ID, "https://github.com/acme/shop/issues/17", ""); err != nil {
		t.Fatal(err)
	}
	task, err := f.db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if err := raiseInvalidPRDecision(ctx, f.db, f.project, task); err != nil {
		t.Fatal(err)
	}
	decisions, err := f.db.Decisions(ctx, f.project.ID, true)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("initial Decision: %#v %v", decisions, err)
	}
	if _, err := f.db.AnswerDecision(ctx, f.project.ID, decisions[0].ID, "reopen-relaunch", "Publish a real PR"); err != nil {
		t.Fatal(err)
	}
	if err := raiseInvalidPRDecision(ctx, f.db, f.project, task); err != nil {
		t.Fatal(err)
	}
	all, err := f.db.Decisions(ctx, f.project.ID, false)
	if err != nil || len(all) != 1 {
		t.Fatalf("answered invalid URL Decision reoffered: %#v %v", all, err)
	}
}
