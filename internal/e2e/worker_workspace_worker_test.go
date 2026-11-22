//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkspaceProjectWorkerUsesOrderedSubworkspaceAndTeardown(t *testing.T) {
	f := newWorkerTabsFixture(t)
	repositoryWorker := f.start(t, "t1", "Repository worker", "repository-worker")
	workspaceRoot := filepath.Join(f.root, "stack")
	member := filepath.Join(workspaceRoot, "backend")
	if err := os.MkdirAll(member, 0o700); err != nil {
		t.Fatal(err)
	}
	initRepository(t, member, filepath.Join(f.root, "backend-origin.git"), f.env)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	workspaceProject, err := db.CreateWorkspaceProject(context.Background(), "stack", workspaceRoot, []store.ProjectRepo{{Name: "backend", Path: "backend", DefaultBranch: "main", OriginHost: "github.com"}})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lead, err := createWorkspace(f.client, workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	leadEnv := setEnv(f.env, "HERDR_ENV", "1")
	leadEnv = setEnv(leadEnv, "HERDR_SOCKET_PATH", herdr.IsolatedTestSocketPath(f.root))
	leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", lead.RootPane.PaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", lead.Workspace.WorkspaceID)
	leadEnv = setEnv(leadEnv, "HERDR_TAB_ID", lead.RootPane.TabID)
	runPosse(t, f.binary, workspaceRoot, leadEnv, "up", "--name", "stack", "--yes")
	if !waitForCondition(10*time.Second, func() bool {
		for _, pane := range f.snapshot(t).Panes {
			if pane.PaneID == lead.RootPane.PaneID && pane.Agent != "" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("Workspace Project Lead did not start")
	}
	brief := filepath.Join(f.root, "workspace-worker.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Workspace worker\ndone_when: workspace worker opens in a sub-workspace\nrepos: [backend]\n---\nWait.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runPosse(t, f.binary, workspaceRoot, leadEnv, "start", "--brief", brief, "--name", "workspace-worker")
	var task store.Task
	if !waitForCondition(20*time.Second, func() bool {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return false
		}
		defer db.Close()
		task, err = db.Task(context.Background(), workspaceProject.ID, "t1")
		return err == nil && task.State == store.StateWorking
	}) {
		t.Fatalf("Workspace Project Worker did not start: %s", output)
	}
	assertOrder := func() {
		t.Helper()
		snapshot := f.snapshot(t)
		ids := make([]string, len(snapshot.Workspaces))
		for i, workspace := range snapshot.Workspaces {
			ids[i] = workspace.WorkspaceID
		}
		for _, pair := range [][2]string{{f.leadWorkspaceID, repositoryWorker.HerdrWorkspaceID}, {lead.Workspace.WorkspaceID, task.HerdrWorkspaceID}} {
			index := -1
			for i, id := range ids {
				if id == pair[0] {
					index = i
					break
				}
			}
			if index < 0 || index+1 >= len(ids) || ids[index+1] != pair[1] {
				t.Fatalf("workspace order = %v, want Worker %s immediately after Lead %s", ids, pair[1], pair[0])
			}
		}
		workspaceFound := false
		workspaceLabelFound := false
		for _, workspace := range snapshot.Workspaces {
			if workspace.WorkspaceID == task.HerdrWorkspaceID {
				workspaceLabelFound = true
				if workspace.Label != "└ workspace-worker-t1" {
					t.Fatalf("Workspace Worker label = %q, want %q", workspace.Label, "└ workspace-worker-t1")
				}
			}
		}
		if !workspaceLabelFound {
			t.Fatalf("Workspace Worker workspace %s is missing", task.HerdrWorkspaceID)
		}
		for _, pane := range snapshot.Panes {
			if pane.WorkspaceID == task.HerdrWorkspaceID && pane.CWD == task.WorktreePath {
				workspaceFound = true
			}
		}
		if !workspaceFound {
			t.Fatalf("Workspace Worker is not anchored at its Sandbox root: %#v", snapshot.Workspaces)
		}
	}
	assertOrder()
	originalWorkspaceID := task.HerdrWorkspaceID
	secondBrief := filepath.Join(f.root, "workspace-worker-second.md")
	if err := os.WriteFile(secondBrief, []byte("---\ntype: change\ntitle: Workspace worker second\ndone_when: workspace workers stay grouped\nrepos: [backend]\n---\nWait.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output = runPosse(t, f.binary, workspaceRoot, leadEnv, "start", "--brief", secondBrief, "--name", "workspace-worker-second")
	var secondTask store.Task
	if !waitForCondition(20*time.Second, func() bool {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return false
		}
		defer db.Close()
		secondTask, err = db.Task(context.Background(), workspaceProject.ID, "t2")
		return err == nil && secondTask.State == store.StateWorking
	}) {
		t.Fatalf("second Workspace Project Worker did not start: %s", output)
	}
	assertGroupedOrder := func() {
		t.Helper()
		snapshot := f.snapshot(t)
		ids := make([]string, len(snapshot.Workspaces))
		for i, workspace := range snapshot.Workspaces {
			ids[i] = workspace.WorkspaceID
			if workspace.WorkspaceID == task.HerdrWorkspaceID && workspace.Label != "└ workspace-worker-t1" {
				t.Fatalf("first Workspace Worker label = %q", workspace.Label)
			}
			if workspace.WorkspaceID == secondTask.HerdrWorkspaceID && workspace.Label != "└ workspace-worker-second-t2" {
				t.Fatalf("second Workspace Worker label = %q", workspace.Label)
			}
		}
		leadIndex := -1
		for i, id := range ids {
			if id == lead.Workspace.WorkspaceID {
				leadIndex = i
				break
			}
		}
		if leadIndex < 0 || leadIndex+2 >= len(ids) || ids[leadIndex+1] != task.HerdrWorkspaceID || ids[leadIndex+2] != secondTask.HerdrWorkspaceID {
			t.Fatalf("Workspace Project sidebar order = %v, want Lead %s followed by %s, %s", ids, lead.Workspace.WorkspaceID, task.HerdrWorkspaceID, secondTask.HerdrWorkspaceID)
		}
	}
	assertGroupedOrder()
	db, err = store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), task.ID, store.StateWorking, store.StateFailed, "worker", "relaunch test"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.WorktreePath, ".posse-e2e-agent-exit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, workspaceRoot, leadEnv, "relaunch", "t1")
	if !waitForCondition(20*time.Second, func() bool {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return false
		}
		defer db.Close()
		current, err := db.Task(context.Background(), workspaceProject.ID, "t1")
		if err != nil || current.State != store.StateWorking {
			return false
		}
		task = current
		return task.HerdrWorkspaceID == originalWorkspaceID
	}) {
		t.Fatalf("Workspace Worker relaunch did not reuse sub-workspace %s", originalWorkspaceID)
	}
	assertOrder()
	assertGroupedOrder()
	killServer(t, f.server)
	f.server = startServer(t, f.client)
	runPosse(t, f.binary, workspaceRoot, f.env, "up", "--all")
	if !waitForCondition(30*time.Second, func() bool {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return false
		}
		defer db.Close()
		task, err = db.Task(context.Background(), workspaceProject.ID, "t1")
		if err != nil || task.State != store.StateWorking {
			return false
		}
		secondTask, err = db.Task(context.Background(), workspaceProject.ID, "t2")
		return err == nil && secondTask.State == store.StateWorking
	}) {
		t.Fatalf("Workspace Project Workers did not restore after Herdr restart: t1=%#v t2=%#v", task, secondTask)
	}
	projectDB, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	workspaceProject, err = projectDB.ProjectByID(context.Background(), workspaceProject.ID)
	if closeErr := projectDB.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	lead.Workspace.WorkspaceID = workspaceProject.HerdrWorkspaceID
	lead.RootPane.PaneID = workspaceProject.LeadPaneID
	leadEnv = setEnv(leadEnv, "HERDR_PANE_ID", workspaceProject.LeadPaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", workspaceProject.HerdrWorkspaceID)
	assertGroupedOrder()
	runPosse(t, f.binary, workspaceRoot, leadEnv, "teardown", "t1", "--discard", "--user-approved", "User approved the Workspace Project child teardown test")
	for _, workspace := range f.snapshot(t).Workspaces {
		if workspace.WorkspaceID == originalWorkspaceID {
			t.Fatalf("Workspace Project teardown left sub-workspace open: %#v", workspace)
		}
	}
	for _, pane := range f.snapshot(t).Panes {
		if pane.WorkspaceID == lead.Workspace.WorkspaceID && pane.PaneID == lead.RootPane.PaneID {
			return
		}
	}
	t.Fatal("Workspace Project teardown closed its Lead")
}
