package app

import (
	"context"
	"errors"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRetainedDiscardWorkerRejectsRelaunch(t *testing.T) {
	fake := herdr.NewFake()
	service := testService(t.TempDir(), fake)
	task := store.Task{Launches: 2, AgentName: "posse-shop-t1-2"}
	intent := store.Intent{PayloadJSON: marshalJSON(discardIntentPayload{
		Discard: true, UserQuote: "destroy t3 now",
		Worker: &discardWorkerPlan{Launch: 1, AgentName: "posse-shop-t1-1"},
	})}
	_, err := service.retainDiscardWorker(context.Background(), nil, store.Project{}, task, intent, taskPaneTeardownPlan{})
	var refusal *axi.Error
	if !errors.As(err, &refusal) || refusal.Code != "discard_agent_identity_unavailable" || len(fake.Calls) != 0 {
		t.Fatalf("retained discard retargeted a relaunch: err=%v calls=%#v", err, fake.Calls)
	}
}

func TestApprovedDiscardRefusesWorkerChangedBeforeIntent(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateWorking)
	attachPRFixtureSandbox(t, f)
	ctx := context.Background()
	original, err := f.db.TaskByID(ctx, f.project.ID, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET launches=launches+1,agent_name='replacement' WHERE id=?`, original.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.teardownTask(ctx, f.db, f.project, config.Config{}, original, true, "destroy t3 now")
	var refusal *axi.Error
	if !errors.As(err, &refusal) || refusal.Code != "discard_agent_identity_unavailable" {
		t.Fatalf("stale cancellation did not refuse relaunch: %v", err)
	}
	sandbox, err := f.db.SandboxByTask(ctx, original.ID)
	if err != nil || sandbox.State != "held" {
		t.Fatalf("stale cancellation changed custody: %#v %v", sandbox, err)
	}
	var approvals int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id=? AND action='discard'`, original.ID).Scan(&approvals); err != nil || approvals != 0 {
		t.Fatalf("stale cancellation recorded approval: %d %v", approvals, err)
	}
}
