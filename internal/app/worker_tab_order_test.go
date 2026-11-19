package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPlaceWorkerWorkspaceAfterExactLeadAndExistingWorkerBlock(t *testing.T) {
	home, db, project, tasks := newWorkerOrderDB(t, []store.Task{
		{Seq: 1, Type: "change", Title: "First", WorktreePath: "/repo/first", HerdrWorkspaceID: "worker-a1"},
		{Seq: 2, Type: "change", Title: "Second", WorktreePath: "/repo/second", HerdrWorkspaceID: "worker-a2"},
		{Seq: 3, Type: "change", Title: "Current", WorktreePath: "/repo/new"},
	})
	defer db.Close()
	adapter := herdr.NewFake()
	adapter.SnapshotValue = herdr.Snapshot{Workspaces: []herdr.Workspace{
		{WorkspaceID: "lead-a"}, {WorkspaceID: "worker-a1"}, {WorkspaceID: "worker-a2"},
		{WorkspaceID: "lead-b"}, {WorkspaceID: "worker-b1"}, {WorkspaceID: "notes"}, {WorkspaceID: "new"},
	}}
	for i, task := range tasks[:2] {
		workspace := &adapter.SnapshotValue.Workspaces[i+1]
		workspace.Worktree.IsLinkedWorktree = true
		workspace.Worktree.CheckoutPath = task.WorktreePath
	}
	adapter.Results["workspace.move"] = json.RawMessage(`{"type":"workspace_list","workspaces":[{"workspace_id":"lead-a"},{"workspace_id":"worker-a1"},{"workspace_id":"worker-a2"},{"workspace_id":"new"},{"workspace_id":"lead-b"},{"workspace_id":"worker-b1"},{"workspace_id":"notes"}]}`)
	service := testService(home, adapter)
	service.placeWorkerWorkspace(context.Background(), db, home, project.ID, tasks[2].ID, "lead-a", "new")
	if adapter.CallCount("workspace.move") != 1 {
		t.Fatalf("workspace.move calls = %d", adapter.CallCount("workspace.move"))
	}
	var move herdr.CallRecord
	for _, call := range adapter.Calls {
		if call.Method == "workspace.move" {
			move = call
		}
	}
	if move.Params["workspace_id"] != "new" || move.Params["insert_index"] != 3 {
		t.Fatalf("workspace.move params = %#v", move.Params)
	}
}

func newWorkerOrderDB(t *testing.T, tasks []store.Task) (string, *store.DB, store.Project, []store.Task) {
	t.Helper()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(context.Background(), "shop", "/repo", "main")
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for i := range tasks {
		id, err := db.CreateTask(context.Background(), project.ID, tasks[i])
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		tasks[i].ID = id
	}
	return home, db, project, tasks
}

func TestOpenWorkerTabContinuesWhenWorkspaceMoveFails(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	root := t.TempDir()
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	storedProject, err := db.CreateProject(context.Background(), project.Name, filepath.Join(root, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	project.ID = storedProject.ID
	storedTaskID, err := db.CreateTask(context.Background(), project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Worker", WorktreePath: task.WorktreePath,
		HerdrWorkspaceID: task.HerdrWorkspaceID, PaneID: task.PaneID, PaneLabel: task.PaneLabel,
	})
	if err != nil {
		t.Fatal(err)
	}
	task.ID = storedTaskID
	adapter.snapshot.Workspaces = append(adapter.snapshot.Workspaces, herdr.Workspace{WorkspaceID: "other"}, herdr.Workspace{WorkspaceID: "new-child"})
	adapter.snapshot.Panes[1].CWD = t.TempDir()
	adapter.Fake.Errors["workspace.move"] = errors.New("unsupported method")
	adapter.Fake.Results["worktree.open"] = json.RawMessage(`{"workspace":{"workspace_id":"new-child"},"tab":{"tab_id":"new-child:t1"},"root_pane":{"pane_id":"new-child:p1","tab_id":"new-child:t1"}}`)
	opened, err := testService(home, adapter).openWorkerTab(context.Background(), db, home, project, task, task.WorktreePath, "claude")
	if err != nil || opened.WorkspaceID != "new-child" {
		t.Fatalf("Worker launch after ordering failure = %#v, %v", opened, err)
	}
	if adapter.CallCount("workspace.move") != 1 {
		t.Fatalf("workspace.move calls = %d", adapter.CallCount("workspace.move"))
	}
}

func TestPlaceWorkerWorkspaceIncludesAnUnrecordedConcurrentWorker(t *testing.T) {
	home, db, project, tasks := newWorkerOrderDB(t, []store.Task{
		{Seq: 1, Type: "change", Title: "First", WorktreePath: "/repo/first"},
		{Seq: 2, Type: "change", Title: "Second", WorktreePath: "/repo/second"},
	})
	defer db.Close()
	adapter := herdr.NewFake()
	adapter.SnapshotValue = herdr.Snapshot{Workspaces: []herdr.Workspace{
		{WorkspaceID: "lead"}, {WorkspaceID: "first-worker"}, {WorkspaceID: "notes"}, {WorkspaceID: "second-worker"},
	}}
	adapter.SnapshotValue.Workspaces[1].Worktree.IsLinkedWorktree = true
	adapter.SnapshotValue.Workspaces[1].Worktree.CheckoutPath = tasks[0].WorktreePath
	adapter.Results["workspace.move"] = json.RawMessage(`{"type":"workspace_list","workspaces":[{"workspace_id":"lead"},{"workspace_id":"first-worker"},{"workspace_id":"second-worker"},{"workspace_id":"notes"}]}`)
	service := testService(home, adapter)
	service.placeWorkerWorkspace(context.Background(), db, home, project.ID, tasks[1].ID, "lead", "second-worker")
	for _, call := range adapter.Calls {
		if call.Method == "workspace.move" {
			if call.Params["insert_index"] != 2 {
				t.Fatalf("second concurrent Worker inserted at %v, want after the first Worker at index 2", call.Params["insert_index"])
			}
			return
		}
	}
	t.Fatal("workspace.move was not called")
}

func TestPlaceWorkerWorkspaceDoesNotTrustAReusedRecordedWorkspaceID(t *testing.T) {
	home, db, project, tasks := newWorkerOrderDB(t, []store.Task{
		{Seq: 1, Type: "change", Title: "Old", WorktreePath: "/repo/old", HerdrWorkspaceID: "reused-id"},
		{Seq: 2, Type: "change", Title: "Current", WorktreePath: "/repo/new"},
	})
	defer db.Close()
	adapter := herdr.NewFake()
	adapter.SnapshotValue = herdr.Snapshot{Workspaces: []herdr.Workspace{
		{WorkspaceID: "lead"}, {WorkspaceID: "reused-id", Root: "/other"}, {WorkspaceID: "notes"}, {WorkspaceID: "new-worker"},
	}}
	adapter.Results["workspace.move"] = json.RawMessage(`{"type":"workspace_list","workspaces":[{"workspace_id":"lead"},{"workspace_id":"new-worker"},{"workspace_id":"reused-id"},{"workspace_id":"notes"}]}`)
	service := testService(home, adapter)
	service.placeWorkerWorkspace(context.Background(), db, home, project.ID, tasks[1].ID, "lead", "new-worker")
	for _, call := range adapter.Calls {
		if call.Method == "workspace.move" {
			if call.Params["insert_index"] != 1 {
				t.Fatalf("Worker inserted at %v after trusting a reused workspace id; want index 1", call.Params["insert_index"])
			}
			return
		}
	}
	t.Fatal("workspace.move was not called")
}

func TestWorkerWorkspaceOrderLockIsSessionScoped(t *testing.T) {
	home := t.TempDir()
	first := workerWorkspaceOrderLockPath(home, "/session-one.sock")
	second := workerWorkspaceOrderLockPath(home, "/session-two.sock")
	if filepath.Clean(first) == filepath.Clean(second) {
		t.Fatalf("different sessions share an ordering lock: %s", first)
	}
}
