package app

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestFocusedPaneBlocksTypingAndNoticeRemainsUndelivered(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p1", Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle", Focused: true}}}
	service := testService(home, fake)
	if err := service.safePrompt(context.Background(), "w1:p1", "never type while focused"); err == nil {
		t.Fatal("safePrompt accepted a focused pane")
	}
	if fake.CallCount("agent.prompt") != 0 {
		t.Fatal("focused pane received typed input")
	}
	if err := service.deliverNotices(context.Background(), db, project); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 0 || fake.CallCount("notification.show") != 1 || fake.CallCount("workspace.report_metadata") != 1 {
		t.Fatalf("focused notice delivery calls = %#v", fake.Calls)
	}
	notices, err := db.UndeliveredNotices(context.Background(), project.ID)
	if err != nil || len(notices) != 1 {
		t.Fatalf("Notice delivery marked open Notice delivered: %#v, %v", notices, err)
	}
}

func TestIngestAppliesBlockedStateAndJournalsEvent(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "Worker started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	event := `{"event":"pane_agent_status_changed","data":{"pane_id":"w2:p1","workspace_id":"w2","agent_status":"blocked","agent":"claude"}}`
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", event)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "blocked"}}}
	service := testService(home, fake)
	output := &bytes.Buffer{}
	contextValue := &axi.Context{Context: context.Background(), Out: output}
	if err := service.ingest(contextValue, nil); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("session.snapshot") != 1 {
		t.Fatalf("_ingest did not reconcile the owned pane: %#v", fake.Calls)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateBlocked {
		t.Fatalf("blocked Task state = %q, %v", task.State, err)
	}
	var events, notices int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM events WHERE kind='pane_agent_status_changed'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='worker_blocked'`, taskID).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if events != 1 || notices != 1 {
		t.Fatalf("event/Notice counts = %d/%d", events, notices)
	}
}

func TestIngestLeadIdleEventDeliversNotice(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "Task t1 is complete"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", `{"event":"pane_agent_status_changed","data":{"pane_id":"w1:p1","workspace_id":"w1","agent_status":"idle"}}`)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p2", Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"},
		{PaneID: "w1:p2", WorkspaceID: "w1", Agent: "claude", AgentStatus: "working", Focused: true},
	}}
	service := testService(home, fake)
	output := &bytes.Buffer{}
	if err := service.ingest(&axi.Context{Context: context.Background(), Out: output}, nil); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("Lead idle event did not deliver the queued Notice: %#v", fake.Calls)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	notices, err := db.UndeliveredNotices(context.Background(), project.ID)
	if err != nil || len(notices) != 0 {
		t.Fatalf("idle Lead Notice delivery state = %#v, %v", notices, err)
	}
}

func TestFocusedPaneDeliversUndeliveredNoticesAcrossProjects(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	projects := []store.Project{}
	for index, name := range []string{"shop", "docs"} {
		project, err := db.CreateProject(context.Background(), name, t.TempDir(), "main")
		if err != nil {
			t.Fatal(err)
		}
		workspace, pane := fmt.Sprintf("w%d", index+1), fmt.Sprintf("w%d:p1", index+1)
		if err := db.SetProjectLead(context.Background(), project.ID, workspace, pane, "posse:"+name+":lead"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: name + " is ready"}); err != nil {
			t.Fatal(err)
		}
		projects = append(projects, project)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PLUGIN_EVENT_JSON", `{"event":"pane_focused","data":{"pane_id":"w1:p1","workspace_id":"w1"}}`)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w3:p1", Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"},
		{PaneID: "w2:p1", WorkspaceID: "w2", Label: "posse:docs:lead", Agent: "claude", AgentStatus: "idle"},
		{PaneID: "w3:p1", WorkspaceID: "w3", Agent: "claude", AgentStatus: "working", Focused: true},
	}}
	service := testService(home, fake)
	if err := service.ingest(&axi.Context{Context: context.Background(), Out: &bytes.Buffer{}}, nil); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 2 {
		t.Fatalf("focused event prompt count = %d; calls=%#v", fake.CallCount("agent.prompt"), fake.Calls)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, project := range projects {
		undelivered, err := db.UndeliveredNotices(context.Background(), project.ID)
		if err != nil || len(undelivered) != 0 {
			t.Fatalf("project %s undelivered Notices = %#v, %v", project.Name, undelivered, err)
		}
	}
}

func TestNoticeBatchNotifiesOnceDeliversOneDigestAndClearsWorkspaceToken(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		if _, err := db.CreateNotice(context.Background(), store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: fmt.Sprintf("Task %d is ready", index+1), DataJSON: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	service := testService(home, fake)
	if err := service.deliverNotices(context.Background(), db, project); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("notification.show") != 1 || fake.CallCount("agent.prompt") != 1 || fake.CallCount("workspace.report_metadata") != 2 {
		t.Fatalf("batch delivery calls = %#v", fake.Calls)
	}
	var clearToken bool
	for _, call := range fake.Calls {
		if call.Method != "workspace.report_metadata" {
			continue
		}
		tokens, ok := call.Params["tokens"].(map[string]string)
		if ok && tokens["posse"] == "" {
			clearToken = true
		}
	}
	if !clearToken {
		t.Fatalf("workspace posse token was not cleared: %#v", fake.Calls)
	}
	undelivered, err := db.UndeliveredNotices(context.Background(), project.ID)
	if err != nil || len(undelivered) != 0 {
		t.Fatalf("delivered batch remains pending: %#v, %v", undelivered, err)
	}
}

func TestQueuedMessageDeliversWhenNextReconcileSeesIdleWorker(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", PaneID: "w2:p1", PaneLabel: "posse:shop:t1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), taskID, store.StateSpawning, store.StateWorking, "cli", "Worker started"); err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(context.Background(), taskID, "Please keep the patch small", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p1", Panes: []herdr.Pane{
		{PaneID: "w1:p1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"},
		{PaneID: "w2:p1", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle"},
	}}
	service := testService(home, fake)
	if err := service.deliverQueuedMessages(context.Background(), db, project, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("idle Worker prompt calls = %d", fake.CallCount("agent.prompt"))
	}
	var status string
	if err := db.QueryRowContext(context.Background(), `SELECT status FROM messages WHERE id=?`, messageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" {
		t.Fatalf("queued message state = %q", status)
	}
}
