package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func newMessageSubmitFixture(t *testing.T, status string) (context.Context, *store.DB, store.Project, int64, *herdr.Fake, *Service) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Fix", MergeMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "Fix the failing lint check", false)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: status}}}
	return ctx, db, project, messageID, fake, testService(home, fake)
}

func messageSubmitStatus(t *testing.T, db *store.DB, messageID int64) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE id=?`, messageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func fakeCalls(fake *herdr.Fake, method string) []herdr.CallRecord {
	var calls []herdr.CallRecord
	for _, call := range fake.Calls {
		if call.Method == method {
			calls = append(calls, call)
		}
	}
	return calls
}

// #436: Claude Code left a multi-line instruction as an unsubmitted
// "[Pasted text #1 +1 lines]" placeholder after swallowing its Enter.
func TestStalledInstructionPastePlaceholderIsSubmittedWithEnter(t *testing.T) {
	ctx, db, project, messageID, fake, service := newMessageSubmitFixture(t, "idle")
	fake.ErrorQueue["agent.prompt"] = []error{&herdr.Error{Code: "agent_prompt_stalled", Message: "agent did not start working"}}
	fake.Results["pane.read"] = json.RawMessage(`{"read":{"text":"> [Pasted text #1 +1 lines]"}}`)

	if err := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	if status := messageSubmitStatus(t, db, messageID); status != "delivered" {
		t.Fatalf("message status=%s, want delivered", status)
	}
	prompts := fakeCalls(fake, "agent.prompt")
	if len(prompts) != 1 {
		t.Fatalf("instruction was typed %d times, want once", len(prompts))
	}
	if wait, ok := prompts[0].Params["wait"].(map[string]any); !ok || !reflect.DeepEqual(wait["until"], []string{"working", "blocked"}) {
		t.Fatalf("idle prompt did not wait for a new turn: %#v", prompts[0].Params)
	}
	keys := fakeCalls(fake, "agent.send_keys")
	if len(keys) != 1 || !reflect.DeepEqual(keys[0].Params["keys"], []string{"enter"}) {
		t.Fatalf("unsubmitted paste was not submitted with one Enter: %#v", keys)
	}
	if fake.CallCount("agent.wait") != 1 {
		t.Fatalf("Enter retry did not wait for the new turn: %d waits", fake.CallCount("agent.wait"))
	}
}

func TestUnsubmittablePastePlaceholderRaisesUncertainty(t *testing.T) {
	ctx, db, project, messageID, fake, service := newMessageSubmitFixture(t, "idle")
	fake.ErrorQueue["agent.prompt"] = []error{&herdr.Error{Code: "agent_prompt_stalled", Message: "agent did not start working"}}
	fake.Results["pane.read"] = json.RawMessage(`{"read":{"text":"> [Pasted text #1 +1 lines]"}}`)
	fake.Errors["agent.wait"] = &herdr.Error{Code: "timeout", Message: "timed out"}

	err := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue)
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "message_delivery_uncertain" {
		t.Fatalf("unconfirmed submission error=%v, want message_delivery_uncertain", err)
	}
	if status := messageSubmitStatus(t, db, messageID); status != "submitting" {
		t.Fatalf("message status=%s, want submitting", status)
	}
	if fake.CallCount("agent.prompt") != 1 || fake.CallCount("agent.send_keys") != messageSubmitAttempts || fake.CallCount("agent.wait") != messageSubmitAttempts {
		t.Fatalf("calls: prompt=%d send_keys=%d wait=%d; want 1 prompt and %d Enter waits", fake.CallCount("agent.prompt"), fake.CallCount("agent.send_keys"), fake.CallCount("agent.wait"), messageSubmitAttempts)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "message_delivery_uncertain" {
		t.Fatalf("unconfirmed submission Notice: %#v, %v", notices, err)
	}
}

// An agent that reports no turn but shows no unsubmitted paste keeps the
// accepted prompt delivered, as before turn confirmation existed.
func TestStalledInstructionWithoutPlaceholderStaysDelivered(t *testing.T) {
	ctx, db, project, messageID, fake, service := newMessageSubmitFixture(t, "idle")
	fake.ErrorQueue["agent.prompt"] = []error{&herdr.Error{Code: "agent_prompt_stalled", Message: "agent did not start working"}}
	fake.Results["pane.read"] = json.RawMessage(`{"read":{"text":"[posse | Lead -> Worker t1 | instruction #1 | turn message:1]"}}`)

	if err := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	if status := messageSubmitStatus(t, db, messageID); status != "delivered" {
		t.Fatalf("message status=%s, want delivered", status)
	}
	if fake.CallCount("agent.prompt") != 1 || fake.CallCount("agent.send_keys") != 0 {
		t.Fatalf("calls: prompt=%d send_keys=%d; want 1, 0", fake.CallCount("agent.prompt"), fake.CallCount("agent.send_keys"))
	}
}

func TestSteeredInstructionDoesNotWaitForNewTurn(t *testing.T) {
	ctx, db, project, messageID, fake, service := newMessageSubmitFixture(t, "working")

	if err := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	if status := messageSubmitStatus(t, db, messageID); status != "delivered" {
		t.Fatalf("message status=%s, want delivered", status)
	}
	prompts := fakeCalls(fake, "agent.prompt")
	if len(prompts) != 1 {
		t.Fatalf("steered instruction prompts=%d, want 1", len(prompts))
	}
	if _, waits := prompts[0].Params["wait"]; waits {
		t.Fatalf("steering a working agent waited for a state change it cannot observe: %#v", prompts[0].Params)
	}
}
