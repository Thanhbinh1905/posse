package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type promptGateAdapter struct {
	herdr.Adapter
	entered chan struct{}
	release chan struct{}
}

func TestConcurrentMessageDeliveryClaimsBeforePrompt(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "Please check the tests", false)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p1", Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", AgentStatus: "working", Focused: true},
		{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle"},
	}}
	adapter := &promptGateAdapter{Adapter: fake, entered: make(chan struct{}, 2), release: make(chan struct{})}
	service := testService(home, adapter)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue)
		}()
	}
	close(start)
	select {
	case <-adapter.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no queued message prompt started")
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("second message delivery did not return while the first prompt was blocked")
	}
	close(adapter.release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("concurrent message delivery prompted more than once: calls=%#v", fake.Calls)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM messages WHERE id=?`, messageID).Scan(&status); err != nil || status != "delivered" {
		t.Fatalf("message state = %q, %v", status, err)
	}
}

func TestNoticeClaimRollsBackWhenPromptFails(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, _ = db.ProjectByID(ctx, project.ID)
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "ready"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	fake.Errors["agent.prompt"] = errors.New("prompt failed")
	service := testService(home, fake)
	if err := service.deliverNotices(ctx, db, project); err == nil {
		t.Fatal("prompt failure was not returned")
	}
	var claims int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE project_id=? AND claim_token<>''`, project.ID).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("failed prompt left %d Notice claims, %v", claims, err)
	}
	fake.Errors["agent.prompt"] = nil
	if err := service.deliverNotices(ctx, db, project); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 2 {
		t.Fatalf("retry prompt calls = %d", fake.CallCount("agent.prompt"))
	}
	undelivered, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(undelivered) != 0 {
		t.Fatalf("retry did not deliver Notice: %#v, %v", undelivered, err)
	}
}

func TestMessageClaimRollsBackWhenPromptFails(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "Please check the tests", false)
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle"}}}
	fake.Errors["agent.prompt"] = errors.New("prompt failed")
	service := testService(home, fake)
	if err := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue); err == nil {
		t.Fatal("prompt failure was not returned")
	}
	var status, token string
	if err := db.QueryRowContext(ctx, `SELECT status,claim_token FROM messages WHERE id=?`, messageID).Scan(&status, &token); err != nil || status != "queued" || token != "" {
		t.Fatalf("failed prompt left message claim state %q/%q, %v", status, token, err)
	}
	fake.Errors["agent.prompt"] = nil
	if err := service.deliverQueuedMessages(ctx, db, project, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status,claim_token FROM messages WHERE id=?`, messageID).Scan(&status, &token); err != nil || status != "delivered" || token != "" {
		t.Fatalf("message retry state = %q/%q, %v", status, token, err)
	}
}

func (a *promptGateAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "agent.prompt" {
		a.entered <- struct{}{}
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return a.Adapter.Call(ctx, method, params)
}

func TestConcurrentNoticeDeliveryClaimsBatchBeforePrompt(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "ready"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	adapter := &promptGateAdapter{Adapter: fake, entered: make(chan struct{}, 2), release: make(chan struct{})}
	service := testService(home, adapter)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- service.deliverNotices(ctx, db, project)
		}()
	}
	close(start)
	select {
	case <-adapter.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("no Notice prompt started")
	}
	select {
	case err := <-results:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("second Notice delivery did not return while the first prompt was blocked")
	}
	close(adapter.release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if got := fake.CallCount("agent.prompt"); got != 1 {
		t.Fatalf("Lead prompt calls = %d, want one", got)
	}
	notices, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(notices) != 0 {
		t.Fatalf("delivered Notice batch remains pending: %#v, %v", notices, err)
	}
}
