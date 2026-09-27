package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestTeardownClosesOnlyVerifiedLinkedChildWithoutGroupFlag(t *testing.T) {
	project, task, adapter := riderTabSession(t)
	task.HerdrWorkspaceID, task.PaneID = "w2", "w2:p1"
	var child herdr.Workspace
	if err := json.Unmarshal([]byte(`{"workspace_id":"w2","label":"first","worktree":{"checkout_path":`+quotedJSON(task.WorktreePath)+`,"is_linked_worktree":true}}`), &child); err != nil {
		t.Fatal(err)
	}
	adapter.snapshot.Workspaces = append(adapter.snapshot.Workspaces, child)
	adapter.snapshot.Panes[2].PaneID = "w2:p1"
	adapter.snapshot.Panes[2].WorkspaceID = "w2"
	adapter.snapshot.Panes[2].TabID = "w2:t1"
	adapter.snapshot.Agents[1].PaneID = "w2:p1"
	adapter.snapshot.FocusedWorkspaceID = "w1"
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

func quotedJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
