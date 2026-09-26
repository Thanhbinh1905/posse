//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// TestRidersOpenAsTabsOfTheLeadWorkspace drives the shared-workspace layout
// through the real CLI against an isolated Herdr with the User's own
// workspaces and tabs around the Lead. Riders must open as tabs of the Lead
// workspace, never create or rename a workspace, and every teardown, relaunch
// and restart path must touch only the Rider's own tab.
func TestRidersOpenAsTabsOfTheLeadWorkspace(t *testing.T) {
	fixture := newRiderTabsFixture(t)
	client := fixture.client

	first := fixture.ride(t, "t1", "Tabs first rider", "tabs-first-rider")
	second := fixture.ride(t, "t2", "Tabs second rider", "tabs-second-rider")
	snapshot := fixture.snapshot(t)
	fixture.assertUserLayout(t, snapshot)
	fixture.assertRiderTab(t, snapshot, first)
	fixture.assertRiderTab(t, snapshot, second)
	fixture.assertRiderLabels(t, snapshot, first, second)
	if tabOf(t, snapshot, first.PaneID) == tabOf(t, snapshot, second.PaneID) {
		t.Fatalf("two Riders share tab %s", tabOf(t, snapshot, first.PaneID))
	}
	if snapshot.FocusedPaneID != fixture.leadPaneID {
		t.Fatalf("ride moved focus off the Lead to %s", snapshot.FocusedPaneID)
	}

	// A foreign split in the first Rider's tab survives its teardown.
	foreign := fixture.split(t, first.PaneID, fixture.root)
	// An unlabeled pane inside the second Mount but in a User workspace is not the Rider's pane;
	// Mount release still stops its shell with every other Mount process.
	stray := fixture.split(t, fixture.userAfter.RootPane.PaneID, second.WorktreePath)
	fixture.fail(t, "t1")
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "unsaddle", "t1", "--discard", "--user-approved", "User approved the tab teardown test")
	if !strings.Contains(output, "foreign_panes[1]: "+strconv.Quote(foreign)) || !strings.Contains(output, "closed_panes[1]: "+strconv.Quote(first.PaneID)) {
		t.Fatalf("teardown with a foreign split reported the wrong panes: %s", output)
	}
	snapshot = fixture.snapshot(t)
	fixture.assertUserLayout(t, snapshot)
	fixture.assertRiderLabels(t, snapshot, second)
	fixture.assertAlive(t, snapshot, foreign, stray, second.PaneID)
	fixture.assertGone(t, snapshot, first.PaneID)

	// The User watches the second Rider; its whole tab closes and focus returns to the Lead.
	ownSplit := fixture.split(t, second.PaneID, second.WorktreePath)
	if _, err := client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": second.PaneID}); err != nil {
		t.Fatal(err)
	}
	secondTab := tabOf(t, fixture.snapshot(t), second.PaneID)
	fixture.fail(t, "t2")
	output = runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "unsaddle", "t2", "--discard", "--user-approved", "User approved the focused tab teardown test")
	if !strings.Contains(output, "foreign_panes[0]") && !strings.Contains(output, "foreign_panes: []") || strings.Contains(output, strconv.Quote(stray)) || !strings.Contains(output, "closed_panes[2]") {
		t.Fatalf("teardown of a Rider tab reported the wrong panes: %s", output)
	}
	snapshot = fixture.snapshot(t)
	fixture.assertUserLayout(t, snapshot)
	fixture.assertRiderLabels(t, snapshot)
	fixture.assertGone(t, snapshot, second.PaneID, ownSplit)
	fixture.assertAlive(t, snapshot, foreign)
	for _, tab := range snapshot.Tabs {
		if tab.TabID == secondTab {
			t.Fatalf("focused Rider tab %s survived teardown", secondTab)
		}
	}
	if snapshot.FocusedPaneID != fixture.leadPaneID {
		t.Fatalf("closing the focused Rider tab left focus on %s, want the Lead %s", snapshot.FocusedPaneID, fixture.leadPaneID)
	}

	// A Rider whose tab was closed by hand relaunches into a new tab of the Lead workspace.
	third := fixture.ride(t, "t3", "Tabs third rider", "tabs-third-rider")
	if _, err := client.Call(context.Background(), "tab.close", map[string]any{"tab_id": tabOf(t, fixture.snapshot(t), third.PaneID)}); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "roster")
	fixture.waitState(t, "t3", store.StateLost)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "relaunch", "t3")
	fixture.waitState(t, "t3", store.StateWorking)
	relaunched := fixture.task(t, "t3")
	snapshot = fixture.snapshot(t)
	fixture.assertUserLayout(t, snapshot)
	fixture.assertRiderTab(t, snapshot, relaunched)
	fixture.assertRiderLabels(t, snapshot, relaunched)

	// After a Herdr restart, recovery reuses the restored tab instead of opening another.
	// Herdr renumbers tab and pane ids on restore, so the tab is compared by its label.
	riderTabs := func(snapshot herdr.Snapshot) (count int) {
		for _, tab := range snapshot.Tabs {
			if tab.Label == "└─ "+relaunched.ShortName {
				count++
			}
		}
		return count
	}
	tabsBefore := len(snapshot.Tabs)
	killServer(t, fixture.server)
	fixture.server = startServer(t, client)
	output = runPosse(t, fixture.binary, fixture.repo, fixture.env, "recover", "--all")
	fixture.waitState(t, "t3", store.StateWorking)
	recovered := fixture.task(t, "t3")
	if recovered.Launches <= relaunched.Launches {
		t.Fatalf("recovery did not relaunch the Rider: before=%d after=%d output=%s", relaunched.Launches, recovered.Launches, output)
	}
	snapshot = fixture.snapshot(t)
	fixture.assertUserLayout(t, snapshot)
	fixture.assertRiderTab(t, snapshot, recovered)
	fixture.assertRiderLabels(t, snapshot, recovered)
	if riderTabs(snapshot) != 1 || len(snapshot.Tabs) != tabsBefore {
		t.Fatalf("recovery opened a new tab: tabs before=%d after=%d: %#v", tabsBefore, len(snapshot.Tabs), snapshot.Tabs)
	}

	// A Task launched before this layout lives alone in its own workspace; it still reconciles and tears down.
	legacy := fixture.ride(t, "t4", "Tabs legacy rider", "tabs-legacy-rider")
	moved, err := client.Call(context.Background(), "pane.move", map[string]any{"pane_id": legacy.PaneID, "destination": map[string]any{"type": "new_workspace", "label": "└─ Tabs legacy rider"}})
	if err != nil {
		t.Fatal(err)
	}
	var movedPane struct {
		MoveResult struct {
			Pane herdr.Pane `json:"pane"`
		} `json:"move_result"`
	}
	if err := json.Unmarshal(moved, &movedPane); err != nil || movedPane.MoveResult.Pane.PaneID == "" || movedPane.MoveResult.Pane.WorkspaceID == fixture.leadWorkspaceID {
		t.Fatalf("could not move the legacy Rider into its own workspace: %s %v", moved, err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "roster")
	current := fixture.task(t, "t4")
	if current.PaneID != movedPane.MoveResult.Pane.PaneID || current.HerdrWorkspaceID != movedPane.MoveResult.Pane.WorkspaceID || current.State != store.StateWorking {
		t.Fatalf("reconcile did not follow the legacy Rider into its workspace: %#v moved=%#v", current, movedPane.MoveResult.Pane)
	}
	fixture.assertRiderLabels(t, fixture.snapshot(t), recovered, current)
	fixture.fail(t, "t4")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "unsaddle", "t4", "--discard", "--user-approved", "User approved the legacy teardown test")
	snapshot = fixture.snapshot(t)
	fixture.assertGone(t, snapshot, movedPane.MoveResult.Pane.PaneID)
	for _, workspace := range snapshot.Workspaces {
		if workspace.WorkspaceID == movedPane.MoveResult.Pane.WorkspaceID {
			t.Fatalf("legacy Rider workspace survived teardown: %#v", workspace)
		}
	}
	fixture.assertUserLayout(t, snapshot)
	fixture.assertRiderLabels(t, snapshot, recovered)
	fixture.assertAlive(t, snapshot, recovered.PaneID, foreign)

}

type riderTabsFixture struct {
	root, repo, home, binary string
	env, leadEnv             []string
	client                   *herdr.Client
	server                   *herdr.ServerProcess
	userBefore, userAfter    workspaceResult
	userTab                  workspaceResult
	leadWorkspaceID          string
	leadPaneID               string
	projectID                int64
}

func newRiderTabsFixture(t *testing.T) *riderTabsFixture {
	t.Helper()
	root := newFixtureRoot(t, fixturePrefix("tabs-"))
	binDir := filepath.Join(root, "bin")
	worktrees := filepath.Join(root, "posse", "remuda")
	for _, directory := range []string{binDir, worktrees, filepath.Join(root, "claude", "skills"), filepath.Join(root, "codex"), filepath.Join(root, "home")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := isolatedE2EEnv(t, root)
	for key, value := range map[string]string{
		"POSSE_TEST_ROOT": root, "POSSE_E2E_WORKTREES": worktrees,
		"PATH":            binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME": "Posse E2E", "GIT_AUTHOR_EMAIL": "posse-e2e@example.test", "GIT_COMMITTER_NAME": "Posse E2E", "GIT_COMMITTER_EMAIL": "posse-e2e@example.test",
	} {
		env = setEnv(env, key, value)
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "posse")
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = moduleRoot(t)
	build.Env = env
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build posse: %v\n%s", err, output)
	}
	// The fake Rider stays working until torn down; the fake Lead just reads.
	agent := `#!/bin/sh
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    while IFS= read -r line; do :; done
    ;;
  *)
    while IFS= read -r line; do :; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(agent), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "posse")
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nlanding_mode = \"local\"\nauto_unsaddle = \"never\"\nmax_workers = 4\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepository(t, repo, filepath.Join(root, "origin.git"), env)

	client := herdr.NewWithEnv("herdr", env)
	server := startServer(t, client)
	if err := client.CheckProtocol(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Run(context.Background(), "integration", "install", "claude"); err != nil {
		t.Fatalf("install Claude integration in isolated Herdr: %v", err)
	}
	// The User's session: a workspace before the Lead, the Lead with a second
	// User tab, and a workspace after it.
	userBefore := createLabeledWorkspace(t, client, root, "notes")
	lead, err := createWorkspace(client, repo)
	if err != nil {
		t.Fatal(err)
	}
	userTab, err := createTab(client, lead.Workspace.WorkspaceID, root, "user-shell")
	if err != nil {
		t.Fatal(err)
	}
	userAfter := createLabeledWorkspace(t, client, root, "scratch")
	if _, err := client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": lead.RootPane.PaneID}); err != nil {
		t.Fatal(err)
	}
	callerEnv := setEnv(env, "HERDR_ENV", "1")
	callerEnv = setEnv(callerEnv, "HERDR_PANE_ID", lead.RootPane.PaneID)
	callerEnv = setEnv(callerEnv, "HERDR_WORKSPACE_ID", lead.Workspace.WorkspaceID)
	callerEnv = setEnv(callerEnv, "HERDR_TAB_ID", lead.RootPane.TabID)
	runPosse(t, binary, repo, callerEnv, "up", "--name", "shop", "--yes")
	db, err := store.OpenReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByName(context.Background(), "shop")
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if project.HerdrWorkspaceID != lead.Workspace.WorkspaceID || project.LeadPaneID != lead.RootPane.PaneID {
		t.Fatalf("Lead did not start in the calling pane: %#v", project)
	}
	return &riderTabsFixture{
		root: root, repo: repo, home: home, binary: binary, env: env,
		leadEnv: callerEnv, client: client, server: server,
		userBefore: userBefore, userAfter: userAfter, userTab: userTab,
		leadWorkspaceID: lead.Workspace.WorkspaceID, leadPaneID: lead.RootPane.PaneID, projectID: project.ID,
	}
}

func createLabeledWorkspace(t *testing.T, client *herdr.Client, cwd, label string) workspaceResult {
	t.Helper()
	raw, err := client.Call(context.Background(), "workspace.create", map[string]any{"cwd": cwd, "label": label, "focus": false})
	if err != nil {
		t.Fatal(err)
	}
	var result workspaceResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *riderTabsFixture) ride(t *testing.T, taskID, title, name string) store.Task {
	t.Helper()
	brief := filepath.Join(f.root, name+".md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: "+title+"\ndone_when: the test tears it down\n---\nWait.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "ride", "--brief", brief, "--name", name); !strings.Contains(output, taskID) {
		t.Fatalf("ride did not return %s: %s", taskID, output)
	}
	f.waitState(t, taskID, store.StateWorking)
	return f.task(t, taskID)
}

func (f *riderTabsFixture) task(t *testing.T, taskID string) store.Task {
	t.Helper()
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(context.Background(), f.projectID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *riderTabsFixture) waitState(t *testing.T, taskID string, want store.State) {
	t.Helper()
	if !waitForCondition(30*time.Second, func() bool {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return false
		}
		defer db.Close()
		task, err := db.Task(context.Background(), f.projectID, taskID)
		return err == nil && task.State == want
	}) {
		t.Fatalf("%s did not reach %s: %#v", taskID, want, f.task(t, taskID))
	}
}

// fail moves a working Task to failed, as a Rider's failed Signal would.
func (f *riderTabsFixture) fail(t *testing.T, taskID string) {
	t.Helper()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(context.Background(), f.projectID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), task.ID, task.State, store.StateFailed, "worker", "test Rider failed"); err != nil {
		t.Fatal(err)
	}
}

func (f *riderTabsFixture) snapshot(t *testing.T) herdr.Snapshot {
	t.Helper()
	snapshot, err := f.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *riderTabsFixture) split(t *testing.T, paneID, cwd string) string {
	t.Helper()
	raw, err := f.client.Call(context.Background(), "pane.split", map[string]any{"target_pane_id": paneID, "direction": "right", "cwd": cwd, "focus": false})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Pane herdr.Pane `json:"pane"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Pane.PaneID == "" {
		t.Fatalf("split pane %s: %s %v", paneID, raw, err)
	}
	return result.Pane.PaneID
}

// assertUserLayout checks that the session still has exactly the User's
// three workspaces, in order and with their labels, and the Lead and the
// User's own tab.
func (f *riderTabsFixture) assertUserLayout(t *testing.T, snapshot herdr.Snapshot) {
	t.Helper()
	var got []string
	for _, workspace := range snapshot.Workspaces {
		got = append(got, workspace.WorkspaceID+"="+workspace.Label)
	}
	want := []string{f.userBefore.Workspace.WorkspaceID + "=notes", f.leadWorkspaceID + "=Lead:shop", f.userAfter.Workspace.WorkspaceID + "=scratch"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Herdr workspaces = %v, want only the User's %v", got, want)
	}
	labels := make(map[string]string, len(snapshot.Tabs))
	for _, tab := range snapshot.Tabs {
		labels[tab.TabID] = tab.Label
	}
	if got := labels[tabOf(t, snapshot, f.leadPaneID)]; got != "Lead" {
		t.Fatalf("Lead tab label = %q, want Lead", got)
	}
	if got := labels[f.userTab.RootPane.TabID]; got != "user-shell" {
		t.Fatalf("User tab label = %q, want user-shell", got)
	}
	f.assertAlive(t, snapshot, f.leadPaneID, f.userTab.RootPane.PaneID, f.userBefore.RootPane.PaneID, f.userAfter.RootPane.PaneID)
}

func (f *riderTabsFixture) assertRiderTab(t *testing.T, snapshot herdr.Snapshot, task store.Task) {
	t.Helper()
	var pane herdr.Pane
	for _, candidate := range snapshot.Panes {
		if candidate.Label == task.PaneLabel {
			pane = candidate
		}
	}
	if pane.PaneID == "" || pane.PaneID != task.PaneID || pane.WorkspaceID != f.leadWorkspaceID || task.HerdrWorkspaceID != f.leadWorkspaceID || pane.CWD != task.WorktreePath {
		t.Fatalf("Rider %s is not a tab of the Lead workspace %s: task=%#v pane=%#v", task.PaneLabel, f.leadWorkspaceID, task, pane)
	}
	leadTab := tabOf(t, snapshot, f.leadPaneID)
	var tabPanes []string
	for _, candidate := range snapshot.Panes {
		if candidate.TabID == pane.TabID {
			tabPanes = append(tabPanes, candidate.PaneID)
		}
	}
	sort.Strings(tabPanes)
	if pane.TabID == leadTab || pane.TabID == f.userTab.RootPane.TabID || len(tabPanes) != 1 {
		t.Fatalf("Rider %s does not own its tab %s: panes=%v lead tab=%s", task.PaneLabel, pane.TabID, tabPanes, leadTab)
	}
	if pane.DisplayAgent != "claude" {
		t.Fatalf("Rider display_agent = %q, want claude", pane.DisplayAgent)
	}
	raw, err := f.client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": pane.PaneID})
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		ProcessInfo struct {
			ShellPID int `json:"shell_pid"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.ProcessInfo.ShellPID == 0 {
		t.Fatalf("Rider pane process info: %s %v", raw, err)
	}
	environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(info.ProcessInfo.ShellPID), "environ"))
	if err != nil {
		t.Fatal(err)
	}
	// Herdr does not persist per-pane env across a restart; recovery then relies on the Mount cwd.
	if !bytes.Contains(environ, []byte("\x00POSSE_WORKER_HOME="+f.home+"\x00")) && task.Launches < 2 {
		t.Fatalf("Rider pane %s was not marked with POSSE_WORKER_HOME=%s", pane.PaneID, f.home)
	}
}

func (f *riderTabsFixture) assertRiderLabels(t *testing.T, snapshot herdr.Snapshot, tasks ...store.Task) {
	t.Helper()
	byTab := make(map[string]store.Task, len(tasks))
	for _, task := range tasks {
		byTab[tabOf(t, snapshot, task.PaneID)] = task
	}
	count := 0
	for _, tab := range snapshot.Tabs {
		task, ok := byTab[tab.TabID]
		if !ok {
			continue
		}
		branch := "├─"
		if count == len(byTab)-1 {
			branch = "└─"
		}
		want := branch + " " + task.ShortName
		if tab.Label != want {
			t.Fatalf("Rider tab label = %q, want %q in Herdr order", tab.Label, want)
		}
		count++
	}
	if count != len(byTab) {
		t.Fatalf("found %d of %d Rider tabs in Herdr tab order", count, len(byTab))
	}
}

func (f *riderTabsFixture) assertAlive(t *testing.T, snapshot herdr.Snapshot, paneIDs ...string) {
	t.Helper()
	for _, paneID := range paneIDs {
		found := false
		for _, pane := range snapshot.Panes {
			found = found || pane.PaneID == paneID
		}
		if !found {
			t.Fatalf("pane %s was closed: %#v", paneID, snapshot.Panes)
		}
	}
}

func (f *riderTabsFixture) assertGone(t *testing.T, snapshot herdr.Snapshot, paneIDs ...string) {
	t.Helper()
	for _, paneID := range paneIDs {
		for _, pane := range snapshot.Panes {
			if pane.PaneID == paneID {
				t.Fatalf("pane %s survived teardown: %#v", paneID, pane)
			}
		}
	}
}

func tabOf(t *testing.T, snapshot herdr.Snapshot, paneID string) string {
	t.Helper()
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return pane.TabID
		}
	}
	t.Fatalf("pane %s is not in the snapshot", paneID)
	return ""
}
