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
	adapter := herdr.NewFake()
	adapter.SnapshotValue = herdr.Snapshot{Workspaces: []herdr.Workspace{
		{WorkspaceID: "lead-a"}, {WorkspaceID: "worker-a1"}, {WorkspaceID: "lead-b"},
		{WorkspaceID: "worker-b1"}, {WorkspaceID: "notes"}, {WorkspaceID: "worker-a2"}, {WorkspaceID: "new"},
	}}
	adapter.Results["workspace.move"] = json.RawMessage(`{"type":"workspace_list","workspaces":[{"workspace_id":"lead-a"},{"workspace_id":"worker-a1"},{"workspace_id":"new"},{"workspace_id":"lead-b"},{"workspace_id":"worker-b1"},{"workspace_id":"notes"},{"workspace_id":"worker-a2"}]}`)
	service := testService(t.TempDir(), adapter)
	service.placeWorkerWorkspace(context.Background(), t.TempDir(), "lead-a", "new", []string{"worker-a1", "worker-a2"})
	if adapter.CallCount("workspace.move") != 1 {
		t.Fatalf("workspace.move calls = %d", adapter.CallCount("workspace.move"))
	}
	var move herdr.CallRecord
	for _, call := range adapter.Calls {
		if call.Method == "workspace.move" {
			move = call
		}
	}
	if move.Params["workspace_id"] != "new" || move.Params["insert_index"] != 2 {
		t.Fatalf("workspace.move params = %#v", move.Params)
	}
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

func TestWorkerWorkspaceOrderLockIsSessionScoped(t *testing.T) {
	home := t.TempDir()
	first := workerWorkspaceOrderLockPath(home, "/session-one.sock")
	second := workerWorkspaceOrderLockPath(home, "/session-two.sock")
	if filepath.Clean(first) == filepath.Clean(second) {
		t.Fatalf("different sessions share an ordering lock: %s", first)
	}
}
