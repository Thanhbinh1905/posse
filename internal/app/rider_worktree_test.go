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

func TestTeardownVerifiesHerdrRemovedOnlyTheLinkedChild(t *testing.T) {
	project, task, adapter := linkedRiderSession(t)
	service := testService(t.TempDir(), adapter)
	plan, err := service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes("w2:p1")
	result, err := service.verifyTaskPanesClosed(context.Background(), project, task, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Closed) != 1 || result.Closed[0] != "w2:p1" || adapter.CallCount("workspace.close") != 0 || adapter.CallCount("tab.close") != 0 || adapter.CallCount("pane.close") != 0 || adapter.CallCount("pane.focus") != 0 {
		t.Fatalf("child teardown: result=%+v calls=%+v", result, adapter.Calls)
	}
	// A stale recorded workspace id cannot claim a reused User workspace.
	project, task, adapter = riderTabSession(t)
	task.HerdrWorkspaceID = "w3"
	task.PaneLabel = "posse:shop:t9"
	if _, err := testService(t.TempDir(), adapter).verifyTaskPanesGone(context.Background(), project, task); err != nil {
		t.Fatal(err)
	}
	if adapter.CallCount("workspace.close") != 0 {
		t.Fatal("stale workspace id closed")
	}
}

func TestForeignPaneKeepsLinkedChildOpen(t *testing.T) {
	project, task, adapter := linkedRiderSession(t)
	adapter.addPane(herdr.Pane{PaneID: "w2:p2", WorkspaceID: "w2", TabID: "w2:t1", Label: "user-notes", CWD: task.WorktreePath})
	service := testService(t.TempDir(), adapter)
	plan, err := service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes("w2:p1")
	result, err := service.verifyTaskPanesClosed(context.Background(), project, task, plan)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.CallCount("workspace.close") != 0 || adapter.CallCount("tab.close") != 0 || adapter.CallCount("pane.close") != 0 || len(result.Foreign) != 1 || result.Foreign[0] != "w2:p2" {
		t.Fatalf("foreign child pane was not preserved and reported: result=%+v calls=%+v", result, adapter.Calls)
	}
}

func TestCrashBetweenWorktreeOpenAndPaneLabelWaitsForHerdrRemoval(t *testing.T) {
	project, task, adapter := linkedRiderSession(t)
	task.ShortName = "first"
	adapter.snapshot.Panes[2].Agent = ""
	adapter.snapshot.Panes[2].Label = ""
	adapter.snapshot.Agents = nil
	service := testService(t.TempDir(), adapter)
	plan, err := service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes("w2:p1")
	result, err := service.verifyTaskPanesClosed(context.Background(), project, task, plan)
	if err != nil || adapter.CallCount("workspace.close") != 0 || len(result.Closed) != 1 {
		t.Fatalf("unlabeled child verification: %+v %v calls=%+v", result, err, adapter.Calls)
	}
	project, task, adapter = linkedRiderSession(t)
	task.ShortName = "first"
	adapter.snapshot.Panes[2].Agent = ""
	adapter.snapshot.Panes[2].Label = ""
	adapter.snapshot.Agents = nil
	adapter.addPane(herdr.Pane{PaneID: "w2:p2", WorkspaceID: "w2", Label: "user-shell", CWD: task.WorktreePath})
	service = testService(t.TempDir(), adapter)
	plan, err = service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes("w2:p1")
	result, err = service.verifyTaskPanesClosed(context.Background(), project, task, plan)
	if err != nil || adapter.CallCount("workspace.close") != 0 || adapter.CallCount("tab.close") != 0 || adapter.CallCount("pane.close") != 0 {
		t.Fatalf("unlabeled child with foreign pane was not preserved: %+v %v calls=%+v", result, err, adapter.Calls)
	}
	for _, pane := range adapter.currentSnapshot().Panes {
		if pane.PaneID == "w2:p2" {
			return
		}
	}
	t.Fatal("foreign pane disappeared during unlabeled-child verification")
}

func TestPartlyRecoveredGroupStillNeedsRecovery(t *testing.T) {
	project, _, adapter := linkedRiderSession(t)
	adapter.snapshot.ServerStartedAt = "same-server"
	adapter.snapshot.Panes = adapter.snapshot.Panes[2:3] // Only a Rider has been restored so far.
	if !riderGroupClosed(adapter.snapshot, project, []store.Task{{ID: 1}}) {
		t.Fatal("partial Rider restore was mistaken for a complete group")
	}
	project.LeadPaneID, project.LeadLabel = "", "" // ensureRecoveryWorkspace already created the Lead workspace.
	if !riderGroupClosed(adapter.snapshot, project, []store.Task{{ID: 1}}) {
		t.Fatal("partial Lead workspace hid an incomplete recovery")
	}
	adapter.snapshot.Panes = nil // The last Rider failed while the group was closed.
	adapter.snapshot.Workspaces = nil
	if !riderGroupClosed(adapter.snapshot, project, nil) {
		t.Fatal("closed group with no live Riders was not recovered")
	}
	adapter.snapshot.Workspaces = []herdr.Workspace{{WorkspaceID: project.HerdrWorkspaceID}}
	if riderGroupClosed(adapter.snapshot, project, nil) {
		t.Fatal("closing just the Lead pane was mistaken for group close")
	}
}
