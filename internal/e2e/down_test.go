//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// userPaneEnv is a User shell in the given workspace, as Herdr exports it.
func (f *riderTabsFixture) userPaneEnv(workspace workspaceResult) []string {
	env := setEnv(f.env, "HERDR_ENV", "1")
	env = setEnv(env, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	env = setEnv(env, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	return setEnv(env, "HERDR_TAB_ID", workspace.RootPane.TabID)
}

func (f *riderTabsFixture) project(t *testing.T) store.Project {
	t.Helper()
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByID(context.Background(), f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// assertLeadStaysGone gives a wrongly started recovery time to show up.
func (f *riderTabsFixture) assertLeadStaysGone(t *testing.T, when string) {
	t.Helper()
	if waitForCondition(3*time.Second, func() bool { _, found := findLeadInSnapshot(f.snapshot(t)); return found }) {
		t.Fatalf("Lead restarted %s", when)
	}
	for _, pane := range f.snapshot(t).Panes {
		if pane.Label == "posse:shop:lookout" {
			t.Fatalf("Lookout restarted %s", when)
		}
	}
}

func (f *riderTabsFixture) upInNewWorkspace(t *testing.T) workspaceResult {
	t.Helper()
	workspace, err := createWorkspace(f.client, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.userPaneEnv(workspace), "up")
	if !waitForCondition(20*time.Second, func() bool {
		lead, found := findLeadInSnapshot(f.snapshot(t))
		return found && lead.PaneID == workspace.RootPane.PaneID
	}) {
		t.Fatalf("posse up did not start the Lead in its caller pane: %#v", f.snapshot(t).Panes)
	}
	if !waitForCondition(30*time.Second, func() bool {
		pids, err := leadFinalizerPIDs(f.root, f.binary)
		return err == nil && len(pids) == 0
	}) {
		t.Fatal("Lead startup finalizer did not finish")
	}
	return workspace
}

func TestDownKeepsProjectStoppedUntilUp(t *testing.T) {
	f := newRiderTabsFixture(t)
	output := runPosse(t, f.binary, f.repo, f.userPaneEnv(f.userBefore), "down")
	if !strings.Contains(output, "state: down") {
		t.Fatalf("down did not report the Project down: %s", output)
	}
	snapshot := f.snapshot(t)
	f.assertGone(t, snapshot, f.leadPaneID)
	f.assertAlive(t, snapshot, f.userTab.RootPane.PaneID, f.userBefore.RootPane.PaneID)
	if project := f.project(t); !project.IsDown() || project.LeadPaneID != "" {
		t.Fatalf("down did not record the Project down: %#v", project)
	}
	if output := runPosse(t, f.binary, f.root, f.env, "--json"); !strings.Contains(output, `"lead":"down"`) {
		t.Fatalf("the dashboard did not show the Lead down: %s", output)
	}
	if output := runPosse(t, f.binary, f.repo, f.env, "recover", "--all"); !strings.Contains(output, "held_projects[1]: shop") {
		t.Fatalf("recover --all did not hold the down Project: %s", output)
	}
	f.assertLeadStaysGone(t, "after recover --all")

	// Closing what is left of the Lead workspace must not bring it back.
	if _, err := f.client.Call(context.Background(), "workspace.close", map[string]any{"workspace_id": f.leadWorkspaceID}); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "recover", "--all")
	runPosse(t, f.binary, f.repo, f.env, "roster")
	f.assertLeadStaysGone(t, "after its workspace closed")

	// A machine restart runs the startup hook against a new Herdr generation.
	killServer(t, f.server)
	f.server = startServer(t, f.client)
	runPosse(t, f.binary, f.repo, f.env, "recover", "--all")
	f.assertLeadStaysGone(t, "after a Herdr restart")

	f.upInNewWorkspace(t)
	if project := f.project(t); project.IsDown() {
		t.Fatalf("posse up left the Project down: %#v", project)
	}
}

func TestDownRefusesRunningTasks(t *testing.T) {
	f := newRiderTabsFixture(t)
	f.ride(t, "t1", "Busy rider", "busy-rider")
	command := exec.Command(f.binary, "down")
	command.Dir, command.Env = f.repo, f.userPaneEnv(f.userBefore)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "tasks_open") || !strings.Contains(string(output), "t1") {
		t.Fatalf("down did not refuse a running Task: %v %s", err, output)
	}
	if project := f.project(t); project.IsDown() {
		t.Fatal("a refused down marked the Project down")
	}
	if _, found := findLeadInSnapshot(f.snapshot(t)); !found {
		t.Fatal("a refused down stopped the Lead")
	}
}

func TestAutoRecoverOffHoldsGroupCloseUntilUp(t *testing.T) {
	f := newRiderTabsFixture(t)
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.auto_recover", "false", "--project", "shop")
	f.ride(t, "t1", "Held rider", "held-rider")
	closeRiderGroup(t, f)
	if output := runPosse(t, f.binary, f.repo, f.env, "recover", "--all"); !strings.Contains(output, "held_projects[1]: shop") {
		t.Fatalf("recover --all did not hold the Project: %s", output)
	}
	runPosse(t, f.binary, f.repo, f.env, "roster")
	f.assertLeadStaysGone(t, "with auto_recover off")
	if task := f.task(t, "t1"); task.State != store.StateWorking {
		t.Fatalf("held recovery changed the Rider's state to %s", task.State)
	}

	workspace := f.upInNewWorkspace(t)
	f.waitState(t, "t1", store.StateWorking)
	task := f.task(t, "t1")
	if !waitForCondition(20*time.Second, func() bool {
		for _, pane := range f.snapshot(t).Panes {
			if pane.PaneID == f.task(t, "t1").PaneID && pane.Agent != "" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("posse up did not relaunch the held Rider: %#v", task)
	}
	if project := f.project(t); project.HerdrWorkspaceID != workspace.Workspace.WorkspaceID {
		t.Fatalf("recovery used workspace %s, not the new Lead's %s", project.HerdrWorkspaceID, workspace.Workspace.WorkspaceID)
	}
}
