package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func linkedRiderSession(t *testing.T) (store.Project, store.Task, *changingSnapshotAdapter) {
	t.Helper()
	project, task, adapter := riderTabSession(t)
	task.HerdrWorkspaceID, task.PaneID = "w2", "w2:p1"
	var child herdr.Workspace
	encodedPath, _ := json.Marshal(task.WorktreePath)
	if err := json.Unmarshal([]byte(`{"workspace_id":"w2","label":"first","worktree":{"checkout_path":`+string(encodedPath)+`,"is_linked_worktree":true}}`), &child); err != nil {
		t.Fatal(err)
	}
	adapter.snapshot.Workspaces = append(adapter.snapshot.Workspaces, child)
	adapter.snapshot.Panes[2].PaneID = "w2:p1"
	adapter.snapshot.Panes[2].WorkspaceID = "w2"
	adapter.snapshot.Panes[2].TabID = "w2:t1"
	adapter.snapshot.Agents[1].PaneID = "w2:p1"
	adapter.snapshot.FocusedWorkspaceID = "w1"
	return project, task, adapter
}

func TestTeardownClosesOnlyVerifiedLinkedChildWithoutGroupFlag(t *testing.T) {
	project, task, adapter := linkedRiderSession(t)
	result, err := testService(t.TempDir(), adapter).closeTaskPanes(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Closed) != 1 || result.Closed[0] != "w2:p1" || adapter.CallCount("workspace.close") != 1 || adapter.CallCount("tab.close") != 0 || adapter.CallCount("pane.focus") != 0 {
		t.Fatalf("child teardown: result=%+v calls=%+v", result, adapter.Calls)
	}
	for _, call := range adapter.Calls {
		if call.Method == "workspace.close" && (call.Params["workspace_id"] != "w2" || call.Params["close_group"] != nil) {
			t.Fatalf("unsafe group close: %+v", call)
		}
	}
	// A stale recorded workspace id cannot claim a reused User workspace.
	project, task, adapter = riderTabSession(t)
	task.HerdrWorkspaceID = "w3"
	task.PaneLabel = "posse:shop:t9"
	if _, err := testService(t.TempDir(), adapter).closeTaskPanes(context.Background(), project, task); err != nil {
		t.Fatal(err)
	}
	if adapter.CallCount("workspace.close") != 0 {
		t.Fatal("stale workspace id closed")
	}
}

func TestForeignPaneKeepsLinkedChildOpen(t *testing.T) {
	project, task, adapter := linkedRiderSession(t)
	adapter.addPane(herdr.Pane{PaneID: "w2:p2", WorkspaceID: "w2", TabID: "w2:t1", Label: "user-notes", CWD: task.WorktreePath})
	result, err := testService(t.TempDir(), adapter).closeTaskPanes(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.CallCount("workspace.close") != 0 || adapter.CallCount("tab.close") != 0 || adapter.CallCount("pane.close") != 1 || len(result.Foreign) != 1 || result.Foreign[0] != "w2:p2" {
		t.Fatalf("foreign child pane was closed: result=%+v calls=%+v", result, adapter.Calls)
	}
}

func TestPartlyRecoveredGroupStillNeedsRecovery(t *testing.T) {
	project, task, adapter := linkedRiderSession(t)
	adapter.snapshot.ServerStartedAt = "same-server"
	adapter.snapshot.Panes = adapter.snapshot.Panes[2:3] // Only a Rider has been restored so far.
	if !riderGroupClosed(adapter.snapshot, project, []store.Task{task}) {
		t.Fatal("partial Rider restore was mistaken for a complete group")
	}
	project.LeadPaneID, project.LeadLabel = "", "" // ensureRecoveryWorkspace already created the Lead workspace.
	if !riderGroupClosed(adapter.snapshot, project, []store.Task{task}) {
		t.Fatal("partial Lead workspace hid an incomplete recovery")
	}
}
