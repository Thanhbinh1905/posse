package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// riderTabSession models a Lead workspace w1 with the Lead tab, a User tab and
// two Worker tabs. The User tab has a shell inside t1's Sandbox.
func workerTabSession(t *testing.T) (store.Project, store.Task, *changingSnapshotAdapter) {
	t.Helper()
	root := t.TempDir()
	sandbox := filepath.Join(root, "sandbox-1")
	sibling := filepath.Join(root, "sandbox-2")
	for _, directory := range []string{sandbox, sibling} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	project := store.Project{Name: "shop", HerdrWorkspaceID: "w1", LeadPaneID: "w1:p1", LeadLabel: "posse:shop:lead"}
	task := store.Task{Seq: 1, WorktreePath: sandbox, HerdrWorkspaceID: "w1", PaneID: "w1:p3", PaneLabel: "posse:shop:t1"}
	adapter := &changingSnapshotAdapter{Fake: herdr.NewFake()}
	adapter.snapshot = herdr.Snapshot{
		FocusedPaneID: "w1:p1",
		Workspaces:    []herdr.Workspace{{WorkspaceID: "w1", Label: "Lead:shop"}},
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Label: "posse:shop:lead", Agent: "claude"},
			{PaneID: "w1:p2", WorkspaceID: "w1", TabID: "w1:t2", CWD: sandbox},
			{PaneID: "w1:p3", WorkspaceID: "w1", TabID: "w1:t3", Label: "posse:shop:t1", CWD: sandbox, Agent: "claude"},
			{PaneID: "w1:p4", WorkspaceID: "w1", TabID: "w1:t4", Label: "posse:shop:t2", CWD: sibling, Agent: "claude"},
		},
		Agents: []herdr.Agent{{Name: "posse-shop-lead-1", PaneID: "w1:p1"}, {Name: "posse-shop-t1-1", PaneID: "w1:p3"}, {Name: "posse-shop-t2-1", PaneID: "w1:p4"}},
	}
	return project, task, adapter
}

func (adapter *changingSnapshotAdapter) addPane(pane herdr.Pane) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.snapshot.Panes = append(adapter.snapshot.Panes, pane)
}

func (adapter *changingSnapshotAdapter) removePanes(paneIDs ...string) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	remove := make(map[string]bool, len(paneIDs))
	for _, paneID := range paneIDs {
		remove[paneID] = true
	}
	panes := adapter.snapshot.Panes[:0]
	for _, pane := range adapter.snapshot.Panes {
		if !remove[pane.PaneID] {
			panes = append(panes, pane)
		}
	}
	adapter.snapshot.Panes = panes
}

func TestTeardownVerifiesHerdrRemovedOnlyTheWorkerTab(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	adapter.addPane(herdr.Pane{PaneID: "w1:p5", WorkspaceID: "w1", TabID: "w1:t3", CWD: task.WorktreePath})
	service := testService(t.TempDir(), adapter)
	plan, err := service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes("w1:p3", "w1:p5") // Herdr removes the Worker tab when its shell exits.
	result, err := service.verifyTaskPanesClosed(context.Background(), project, task, plan)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.CallCount("tab.close") != 0 || adapter.CallCount("pane.close") != 0 || adapter.CallCount("workspace.close") != 0 {
		t.Fatalf("Teardown issued an unconditional close RPC: %#v", adapter.Calls)
	}
	if !reflect.DeepEqual(result.Closed, []string{"w1:p3", "w1:p5"}) || len(result.Foreign) != 0 {
		t.Fatalf("teardown result = %#v", result)
	}
	var left []string
	for _, pane := range adapter.currentSnapshot().Panes {
		left = append(left, pane.PaneID)
	}
	// The User's shell inside the Sandbox is in another tab and is not the Worker's.
	if !reflect.DeepEqual(left, []string{"w1:p1", "w1:p2", "w1:p4"}) {
		t.Fatalf("panes left after Herdr removed the Worker tab = %v", left)
	}
}

func TestTeardownReportsForeignPanesAfterHerdrRemovesTheWorkerPane(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	adapter.addPane(herdr.Pane{PaneID: "w1:p5", WorkspaceID: "w1", TabID: "w1:t3", CWD: "/"})
	adapter.addPane(herdr.Pane{PaneID: "w1:p6", WorkspaceID: "w1", TabID: "w1:t3", CWD: task.WorktreePath, Agent: "claude"})
	service := testService(t.TempDir(), adapter)
	plan, err := service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes("w1:p3")
	result, err := service.verifyTaskPanesClosed(context.Background(), project, task, plan)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.CallCount("tab.close") != 0 || adapter.CallCount("pane.close") != 0 || adapter.CallCount("workspace.close") != 0 {
		t.Fatalf("Teardown issued an unconditional close RPC: %#v", adapter.Calls)
	}
	// Siblings, the Lead and the User tab are not listed: only the Worker tab's other panes are.
	if !reflect.DeepEqual(result.Closed, []string{"w1:p3"}) || !reflect.DeepEqual(result.Foreign, []string{"w1:p5", "w1:p6"}) {
		t.Fatalf("teardown result = %#v", result)
	}
}

func TestTeardownAcceptsTaskPaneRemovedBeforeVerification(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	service := testService(t.TempDir(), adapter)
	plan, err := service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes(task.PaneID) // Herdr removed the tab before verification.
	if _, err := service.verifyTaskPanesClosed(context.Background(), project, task, plan); err != nil {
		t.Fatalf("a pane removed by Herdr failed teardown: %v", err)
	}
	if adapter.CallCount("pane.close") != 0 || adapter.CallCount("tab.close") != 0 || adapter.CallCount("workspace.close") != 0 {
		t.Fatalf("Teardown issued an unconditional close RPC: %#v", adapter.Calls)
	}
	if !missingPaneError(&herdr.Error{Code: "tab_not_found"}) || !missingPaneError(axi.Failure("tab_not_found", "gone", false)) {
		t.Fatal("tab_not_found is not treated as already closed")
	}
}

func TestTeardownReturnsFocusToTheLead(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	adapter.snapshot.FocusedPaneID = "w1:p3"
	service := testService(t.TempDir(), adapter)
	plan, err := service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes(task.PaneID)
	if _, err := service.verifyTaskPanesClosed(context.Background(), project, task, plan); err != nil {
		t.Fatal(err)
	}
	focused := ""
	for _, call := range adapter.Calls {
		if call.Method == "pane.focus" {
			focused, _ = call.Params["pane_id"].(string)
		}
	}
	if focused != "w1:p1" {
		t.Fatalf("closing the focused Worker tab focused %q, want the Lead: %#v", focused, adapter.Calls)
	}

	project, task, adapter = workerTabSession(t)
	service = testService(t.TempDir(), adapter)
	plan, err = service.planTaskPaneTeardown(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	adapter.removePanes(task.PaneID)
	if _, err := service.verifyTaskPanesClosed(context.Background(), project, task, plan); err != nil {
		t.Fatal(err)
	}
	if adapter.CallCount("pane.focus") != 0 {
		t.Fatalf("teardown of an unfocused Worker moved focus: %#v", adapter.Calls)
	}
}

// Herdr renumbers pane ids on restore and reuses workspace ids, so a recorded
// id alone never makes a pane the Task's.
func TestStaleRecordedIdsDoNotClaimAnotherPane(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	task.PaneLabel = "posse:shop:t9"
	task.PaneID = "w1:p4"
	if _, found := findTaskPane(adapter.snapshot.Panes, task); found {
		t.Fatal("a sibling Worker's pane was found by a stale pane id")
	}
	if _, found := findAppPane(adapter.snapshot.Panes, "w1:p4", "posse:shop:t9"); found {
		t.Fatal("findAppPane matched a labeled pane by a stale id")
	}
	task.PaneID = "w1:p2"
	if _, found := findTaskPane(adapter.snapshot.Panes, task); !found {
		t.Fatal("an unlabeled pane inside the Sandbox with the recorded id was not found")
	}
	result, err := testService(t.TempDir(), adapter).verifyTaskPanesGone(context.Background(), project, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(adapter.Calls) != 0 || len(result.Closed) != 0 || len(result.Foreign) != 0 {
		t.Fatalf("teardown without the Task's labeled pane closed something: %#v %#v", result, adapter.Calls)
	}
}

func TestLeadWorkspaceRequiresTheLeadOrItsLabel(t *testing.T) {
	project := store.Project{Name: "shop", HerdrWorkspaceID: "w1", LeadPaneID: "w1:p1", LeadLabel: "posse:shop:lead"}
	moved := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w4:p1", WorkspaceID: "w4", Label: "posse:shop:lead"}}}
	if got, found := leadWorkspace(moved, project); !found || got != "w4" {
		t.Fatalf("Lead workspace after the Lead pane moved = %q %v", got, found)
	}
	// After a restart w1 names a User workspace whose pane now has id w1:p1.
	reused := herdr.Snapshot{Workspaces: []herdr.Workspace{{WorkspaceID: "w1", Label: "notes"}}, Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "user-notes"}}}
	if got, found := leadWorkspace(reused, project); found {
		t.Fatalf("a reused workspace id was adopted as the Lead's: %q", got)
	}
	recreated := herdr.Snapshot{Workspaces: []herdr.Workspace{{WorkspaceID: "w1", Label: "Lead:shop"}}}
	if got, found := leadWorkspace(recreated, project); !found || got != "w1" {
		t.Fatalf("the recovered Lead workspace was not accepted: %q %v", got, found)
	}
}

func TestOpenWorkerTabUsesTheLeadWorkspace(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	adapter.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w1:t5"},"root_pane":{"pane_id":"w1:p5","tab_id":"w1:t5"}}`)
	task.ShortName = "first-worker"
	home := t.TempDir()
	opened, err := testService(home, adapter).openWorkerTab(context.Background(), home, project, task, task.WorktreePath, "claude")
	if err != nil || opened != (openedTab{WorkspaceID: "w1", TabID: "w1:t5", PaneID: "w1:p5"}) {
		t.Fatalf("openWorkerTab = %#v, %v", opened, err)
	}
	var methods []string
	for _, call := range adapter.Calls {
		methods = append(methods, call.Method)
		if call.Method == "tab.create" {
			if call.Params["workspace_id"] != "w1" || call.Params["cwd"] != task.WorktreePath || call.Params["label"] != "first-worker" || call.Params["focus"] != false || call.Params["env"].(map[string]string)[workerHomeEnv] != filepath.Clean(home) {
				t.Fatalf("tab.create params = %#v", call.Params)
			}
		}
		if call.Method == "pane.rename" && (call.Params["pane_id"] != "w1:p5" || call.Params["label"] != task.PaneLabel) {
			t.Fatalf("pane.rename params = %#v", call.Params)
		}
		if call.Method == "pane.report_metadata" && (call.Params["pane_id"] != "w1:p5" || call.Params["display_agent"] != "claude" || call.Params["tokens"].(map[string]string)["posse_row"] != "└─ first-worker") {
			t.Fatalf("initial Worker metadata = %#v", call.Params)
		}
	}
	if strings.Join(methods, ",") != "tab.create,pane.report_metadata,pane.rename" {
		t.Fatalf("openWorkerTab calls = %v", methods)
	}

	// Without the Lead, posse refuses rather than open a second workspace.
	adapter = &changingSnapshotAdapter{Fake: herdr.NewFake(), snapshot: herdr.Snapshot{Workspaces: []herdr.Workspace{{WorkspaceID: "w1", Label: "notes"}}}}
	_, err = testService(home, adapter).openWorkerTab(context.Background(), home, project, task, task.WorktreePath, "claude")
	var cliErr *axi.Error
	if !errors.As(err, &cliErr) || cliErr.Code != "lead_missing" || adapter.CallCount("tab.create") != 0 || adapter.CallCount("worktree.open") != 0 || adapter.CallCount("workspace.create") != 0 {
		t.Fatalf("openWorkerTab without the Lead = %v, calls %#v", err, adapter.Calls)
	}
}

func TestOpenWorkerTabRejectsAnOccupiedLegacySandboxButAllowsAnEmptyWorkspace(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	legacy := herdr.Workspace{WorkspaceID: "legacy"}
	legacy.Worktree.CheckoutPath = task.WorktreePath
	legacy.Worktree.IsLinkedWorktree = true
	adapter.snapshot.Workspaces = append(adapter.snapshot.Workspaces, legacy)
	adapter.snapshot.Panes = append(adapter.snapshot.Panes, herdr.Pane{
		PaneID: "legacy:p1", WorkspaceID: "legacy", TabID: "legacy:t1", CWD: task.WorktreePath,
	})

	service := testService(t.TempDir(), adapter)
	_, err := service.openWorkerTab(context.Background(), t.TempDir(), project, task, task.WorktreePath, "claude")
	var cliErr *axi.Error
	if !errors.As(err, &cliErr) || cliErr.Code != "worker_workspace_occupied" || adapter.CallCount("tab.create") != 0 {
		t.Fatalf("openWorkerTab with an occupied legacy workspace = %v, calls %#v", err, adapter.Calls)
	}

	adapter.snapshot.Panes = adapter.snapshot.Panes[:len(adapter.snapshot.Panes)-1]
	_, err = service.openWorkerTab(context.Background(), t.TempDir(), project, task, task.WorktreePath, "claude")
	if err != nil || adapter.CallCount("tab.create") != 1 {
		t.Fatalf("openWorkerTab with an empty legacy workspace = %v, calls %#v", err, adapter.Calls)
	}
}

func TestOpenWorkerTabDoesNotCloseAnUnlabeledPaneAfterMetadataFailure(t *testing.T) {
	project, task, adapter := workerTabSession(t)
	adapter.Errors["pane.report_metadata"] = errors.New("metadata write failed")
	_, err := testService(t.TempDir(), adapter).openWorkerTab(context.Background(), t.TempDir(), project, task, task.WorktreePath, "claude")
	if err == nil {
		t.Fatal("openWorkerTab succeeded after metadata failure")
	}
	for _, method := range []string{"pane.close", "tab.close", "workspace.close"} {
		if adapter.CallCount(method) != 0 {
			t.Fatalf("failed Worker open issued an unconditional %s RPC: %#v", method, adapter.Calls)
		}
	}
}

func TestRefreshWorkerDisplayNeverRenamesWorkspaces(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "shop"), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Show Worker worktrees", ShortName: "show-worktrees", MergeMode: "local", WorktreePath: "/tmp/pool/sandbox-1", HerdrWorkspaceID: "w1", PaneID: "w1:p3", PaneLabel: "posse:shop:t1"})
	if err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue.Panes = []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead"}, {PaneID: "w1:p3", WorkspaceID: "w1", TabID: "w1:t3", Label: "posse:shop:t1", Agent: "claude"}}
	if err := testService(home, fake).refreshWorkerDisplay(ctx, db, project, id, "claude"); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("workspace.rename") != 0 || fake.CallCount("pane.report_metadata") != 1 {
		t.Fatalf("Worker display calls = %#v", fake.Calls)
	}
	for _, call := range fake.Calls {
		if call.Method == "pane.report_metadata" && (call.Params["pane_id"] != "w1:p3" || call.Params["display_agent"] != "claude") {
			t.Fatalf("Worker metadata = %#v", call.Params)
		}
	}
}

func TestOwnsTaskPaneMatchesNamelessAgentByRecordedSession(t *testing.T) {
	sandbox := t.TempDir()
	project := store.Project{Name: "clouddbv1"}
	task := store.Task{
		Seq:              15,
		WorktreePath:     sandbox,
		PaneID:           "wT8:pM",
		PaneLabel:        "posse:clouddbv1:t15",
		AgentName:        "posse-clouddbv1-t15-1",
		AgentSession:     `{"session_id":"01a1425b-3893-75f3-8310-79ae9e6f9b49"}`,
		AgentSessionKind: "codex",
	}
	pane := herdr.Pane{
		PaneID:       task.PaneID,
		WorkspaceID:  "wT8",
		TabID:        "wT8:tM",
		Label:        task.PaneLabel,
		CWD:          sandbox,
		Agent:        "codex",
		AgentSession: json.RawMessage(`{"agent":"codex","kind":"id","source":"herdr:codex","value":"01a1425b-3893-75f3-8310-79ae9e6f9b49"}`),
	}
	snapshot := herdr.Snapshot{
		Panes:  []herdr.Pane{pane},
		Agents: []herdr.Agent{{PaneID: pane.PaneID, Agent: "codex", SessionID: "01a1425b-3893-75f3-8310-79ae9e6f9b49"}},
	}
	if !ownsTaskPane(snapshot, project, task, nil, pane) {
		t.Fatal("nameless agent with the Task's exact pane, Sandbox cwd, kind, and session was not recognized")
	}
	paneOnlySession := herdr.Snapshot{Panes: []herdr.Pane{pane}, Agents: []herdr.Agent{{PaneID: pane.PaneID, Agent: "codex"}}}
	if !ownsTaskPane(paneOnlySession, project, task, nil, pane) {
		t.Fatal("nameless agent with the exact session only on the pane was not recognized")
	}
	agentOnlySessionPane := pane
	agentOnlySessionPane.AgentSession = nil
	if !ownsTaskPane(herdr.Snapshot{Panes: []herdr.Pane{agentOnlySessionPane}, Agents: snapshot.Agents}, project, task, nil, agentOnlySessionPane) {
		t.Fatal("nameless agent with the exact session only on the agent was not recognized")
	}
	conflictingSessionPane := pane
	conflictingSessionPane.AgentSession = json.RawMessage(`{"agent":"codex","kind":"id","source":"herdr:codex","value":"replacement-session"}`)
	if ownsTaskPane(herdr.Snapshot{Panes: []herdr.Pane{conflictingSessionPane}, Agents: snapshot.Agents}, project, task, nil, conflictingSessionPane) {
		t.Fatal("conflicting Herdr session records were claimed")
	}

	for name, replacement := range map[string]herdr.Agent{
		"different session":     {PaneID: pane.PaneID, Agent: "codex", SessionID: "replacement-session"},
		"different harness":     {PaneID: pane.PaneID, Agent: "claude", SessionID: "01a1425b-3893-75f3-8310-79ae9e6f9b49"},
		"different named agent": {Name: "replacement-agent", PaneID: pane.PaneID, Agent: "codex", SessionID: "01a1425b-3893-75f3-8310-79ae9e6f9b49"},
	} {
		t.Run(name, func(t *testing.T) {
			if ownsTaskPane(herdr.Snapshot{Panes: []herdr.Pane{pane}, Agents: []herdr.Agent{replacement}}, project, task, nil, pane) {
				t.Fatalf("replacement agent was claimed: %#v", replacement)
			}
		})
	}
	ambiguousAgents := append(append([]herdr.Agent(nil), snapshot.Agents...), herdr.Agent{Name: "replacement-agent", PaneID: pane.PaneID, Agent: "codex", SessionID: "replacement-session"})
	if ownsTaskPane(herdr.Snapshot{Panes: []herdr.Pane{pane}, Agents: ambiguousAgents}, project, task, nil, pane) {
		t.Fatal("ambiguous Herdr agent records were claimed")
	}

	outside := pane
	outside.CWD = t.TempDir()
	if ownsTaskPane(herdr.Snapshot{Panes: []herdr.Pane{outside}, Agents: snapshot.Agents}, project, task, nil, outside) {
		t.Fatal("nameless agent outside the Task Sandbox was claimed")
	}
	otherPane := pane
	otherPane.PaneID = "wT8:pN"
	if ownsTaskPane(herdr.Snapshot{Panes: []herdr.Pane{otherPane}, Agents: snapshot.Agents}, project, task, nil, otherPane) {
		t.Fatal("agent in a replacement pane was claimed")
	}
}

func TestAgentNameMatchesTaskFollowsAgentNameLimits(t *testing.T) {
	project := "start-agent.start-after-a-long-project"
	if name := agentName(project, 1, 2); !agentNameMatchesTask(name, project, 1) {
		t.Fatalf("launch name %q of a long, sanitized Project name was not the Task's", name)
	}
	for _, name := range []string{agentName(project, 11, 2), agentName("shop", 1, 2), "posse-shop-t1-", "posse-shop-t1-x", "posse-shop-t1-02"} {
		if agentNameMatchesTask(name, project, 1) || (strings.HasPrefix(name, "posse-shop") && agentNameMatchesTask(name, "shop", 3)) {
			t.Fatalf("agent %q was taken for Task t1", name)
		}
	}
}
