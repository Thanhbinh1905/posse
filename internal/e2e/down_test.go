//go:build e2e

package e2e

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// userPaneEnv is a User shell in the given workspace, as Herdr exports it.
func (f *workerTabsFixture) userPaneEnv(workspace workspaceResult) []string {
	env := setEnv(f.env, "HERDR_ENV", "1")
	env = setEnv(env, "HERDR_SOCKET_PATH", herdr.IsolatedTestSocketPath(f.root))
	env = setEnv(env, "HERDR_PANE_ID", workspace.RootPane.PaneID)
	env = setEnv(env, "HERDR_WORKSPACE_ID", workspace.Workspace.WorkspaceID)
	return setEnv(env, "HERDR_TAB_ID", workspace.RootPane.TabID)
}

func (f *workerTabsFixture) project(t *testing.T) store.Project {
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
func (f *workerTabsFixture) assertLeadStaysGone(t *testing.T, when string) {
	t.Helper()
	if waitForCondition(3*time.Second, func() bool { _, found := findLeadInSnapshot(f.snapshot(t)); return found }) {
		t.Fatalf("Lead restarted %s", when)
	}
	for _, pane := range f.snapshot(t).Panes {
		if pane.Label == "posse:shop:watch" {
			t.Fatalf("Watch restarted %s", when)
		}
	}
}

func (f *workerTabsFixture) upInNewWorkspace(t *testing.T) workspaceResult {
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
	f := newWorkerTabsFixture(t)
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
	if pids := watchPIDs(f.root); len(pids) != 0 {
		t.Fatalf("down left hidden Watch runtimees running: %v", pids)
	}
	if output := runPosse(t, f.binary, f.root, f.env, "--json"); !strings.Contains(output, `"lead":"down"`) {
		t.Fatalf("the dashboard did not show the Lead down: %s", output)
	}
	if output := runPosse(t, f.binary, f.repo, f.env, "up", "--all"); !strings.Contains(output, "down_projects[1]: shop") {
		t.Fatalf("up --all did not skip the down Project: %s", output)
	}
	f.assertLeadStaysGone(t, "after up --all")

	// Closing what is left of the Lead workspace must not bring it back.
	if _, err := f.client.Call(context.Background(), "workspace.close", map[string]any{"workspace_id": f.leadWorkspaceID}); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "up", "--all")
	runPosse(t, f.binary, f.repo, f.env, "list")
	f.assertLeadStaysGone(t, "after its workspace closed")

	// A machine restart runs the startup hook against a new Herdr generation.
	killServer(t, f.server)
	f.server = startServer(t, f.client)
	runPosse(t, f.binary, f.repo, f.env, "_herdr_started")
	runPosse(t, f.binary, f.repo, f.env, "up", "--all")
	f.assertLeadStaysGone(t, "after a Herdr restart")

	f.upInNewWorkspace(t)
	if project := f.project(t); project.IsDown() {
		t.Fatalf("posse up left the Project down: %#v", project)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("posse up did not restart exactly one hidden watch: %v", watchPIDs(f.root))
	}
}

func TestDownRefusesRunningTasks(t *testing.T) {
	f := newWorkerTabsFixture(t)
	f.start(t, "t1", "Busy worker", "busy-worker")
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

func TestGroupCloseHoldsUntilUp(t *testing.T) {
	f := newWorkerTabsFixture(t)
	f.start(t, "t1", "Held worker", "held-worker")
	closeWorkerGroup(t, f)
	if output := runPosse(t, f.binary, f.repo, f.env, "_herdr_started"); !strings.Contains(output, "held_projects[1]: shop") {
		t.Fatalf("the startup hook did not hold the Project: %s", output)
	}
	runPosse(t, f.binary, f.repo, f.env, "list")
	f.assertLeadStaysGone(t, "after a group close")
	if task := f.task(t, "t1"); task.State != store.StateWorking {
		t.Fatalf("held restore changed the Worker's state to %s", task.State)
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
		t.Fatalf("posse up did not relaunch the held Worker: %#v", task)
	}
	if project := f.project(t); project.HerdrWorkspaceID != workspace.Workspace.WorkspaceID {
		t.Fatalf("recovery used workspace %s, not the new Lead's %s", project.HerdrWorkspaceID, workspace.Workspace.WorkspaceID)
	}
}

// TestDownExecuteFromLeadShutsEverythingDown is the Lead path of `/posse-down`:
// a plan that changes nothing, then one approved execute from the live Lead.
func TestDownExecuteFromLeadShutsEverythingDown(t *testing.T) {
	f := newQueuedAdmissionFixture(t)
	runPosseExpectingFailure(t, f.workerTabsFixture, f.repo, f.leadEnv, "user_only", "down")
	runPosseExpectingFailure(t, f.workerTabsFixture, f.repo, f.leadEnv, "down_approval_required", "down", "--execute")

	plan := runPosse(t, f.binary, f.repo, f.leadEnv, "down", "--plan")
	for _, want := range []string{"t1,working,Holds the slot", "t2,queued,Queued worker", "work_lost", "kept_decisions", "--execute --user-approved"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("down --plan did not show %q: %s", want, plan)
		}
	}
	if project := f.project(t); project.IsDown() {
		t.Fatal("down --plan marked the Project down")
	}
	if first, second := f.task(t, "t1"), f.task(t, "t2"); first.State != store.StateWorking || second.State != store.StateQueued {
		t.Fatalf("down --plan changed Task states: t1=%s t2=%s", first.State, second.State)
	}
	if _, found := findLeadInSnapshot(f.snapshot(t)); !found || len(watchPIDs(f.root)) == 0 {
		t.Fatal("down --plan stopped the Lead or the Watch")
	}

	quote := "ok, shut posse down"
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "down", "--execute", "--user-approved", quote)
	if !strings.Contains(output, "state: down") {
		t.Fatalf("down --execute did not report the Project down: %s", output)
	}
	if project := f.project(t); !project.IsDown() || project.LeadPaneID != "" {
		t.Fatalf("down --execute did not record the Project down: %#v", project)
	}
	for _, id := range []string{"t1", "t2"} {
		if task := f.task(t, id); task.State != store.StateTornDown {
			t.Fatalf("down --execute left %s in %s", id, task.State)
		}
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := db.LatestApprovalQuote(context.Background(), f.task(t, "t1").ID, "discard")
	_ = db.Close()
	if err != nil || recorded != quote {
		t.Fatalf("discard approval quote = %q, %v; want %q", recorded, err, quote)
	}
	if pids := watchPIDs(f.root); len(pids) != 0 {
		t.Fatalf("down --execute left Watch processes running: %v", pids)
	}
	snapshot := f.snapshot(t)
	f.assertGone(t, snapshot, f.leadPaneID)
	f.assertAlive(t, snapshot, f.userTab.RootPane.PaneID, f.userBefore.RootPane.PaneID)
	f.assertLeadStaysGone(t, "after down --execute")

	f.upInNewWorkspace(t)
	if project := f.project(t); project.IsDown() {
		t.Fatalf("posse up left the Project down: %#v", project)
	}
}

func TestDownExecuteRefusesMergingTaskAndWorkers(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Merging worker", "merging-worker")
	for _, args := range [][]string{{"down", "--plan"}, {"down", "--execute", "--user-approved", "ok"}} {
		runPosseExpectingFailure(t, f, f.repo, workerSignalEnv(f, task), "worker_forbidden", args...)
	}

	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		from, to store.State
		source   string
	}{{store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateMerging, "cli"}} {
		if err := db.Transition(context.Background(), task.ID, step.from, step.to, step.source, "fixture merging"); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	if plan := runPosse(t, f.binary, f.repo, f.leadEnv, "down", "--plan"); !strings.Contains(plan, "refuses while t1 is merging") {
		t.Fatalf("down --plan did not warn about the merging Task: %s", plan)
	}
	runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "tasks_merging", "down", "--execute", "--user-approved", "ok")
	if project := f.project(t); project.IsDown() {
		t.Fatal("a refused down --execute marked the Project down")
	}
	if current := f.task(t, "t1"); current.State != store.StateMerging {
		t.Fatalf("a refused down --execute changed t1 to %s", current.State)
	}
	if _, found := findLeadInSnapshot(f.snapshot(t)); !found {
		t.Fatal("a refused down --execute stopped the Lead")
	}
}
