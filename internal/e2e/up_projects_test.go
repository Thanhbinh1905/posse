//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// leadRunning reports whether Project name has a Lead agent in Herdr.
func (f *workerTabsFixture) leadRunning(t *testing.T, name string) bool {
	t.Helper()
	for _, pane := range f.snapshot(t).Panes {
		if pane.Label == "posse:"+name+":lead" && pane.Agent != "" {
			return true
		}
	}
	return false
}

// startProject registers Project name from a new repository and starts its
// Lead the way the User does: `posse up` in a shell of its folder.
func (f *workerTabsFixture) startProject(t *testing.T, name string) {
	t.Helper()
	repo := filepath.Join(f.root, "projects", name)
	initRepository(t, repo, filepath.Join(f.root, "origins", name+".git"), f.env)
	workspace, err := createWorkspace(f.client, repo)
	if err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, repo, f.userPaneEnv(workspace), "up", "--name", name, "--yes")
	if !waitForCondition(20*time.Second, func() bool { return f.leadRunning(t, name) }) {
		t.Fatalf("posse up did not start the %s Lead: %#v", name, f.snapshot(t).Panes)
	}
	if !waitForCondition(30*time.Second, func() bool {
		pids, err := leadFinalizerPIDs(f.root, f.binary)
		return err == nil && len(pids) == 0
	}) {
		t.Fatalf("%s Lead startup finalizer did not finish", name)
	}
}

func TestUpAllRestoresOnlyUpProjects(t *testing.T) {
	f := newWorkerTabsFixture(t)
	f.startProject(t, "posse")
	f.startProject(t, "clouddbv1")

	killServer(t, f.server)
	f.server = startServer(t, f.client)
	runPosse(t, f.binary, f.repo, f.env, "_herdr_started")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "up.projects", `["posse", "clouddbv1"]`)

	output := runPosse(t, f.binary, f.repo, f.env, "up", "--all")
	if !strings.Contains(output, "restored[2]") || !strings.Contains(output, "skipped_projects[1]: shop") {
		t.Fatalf("up --all did not restore only up.projects: %s", output)
	}
	if !waitForCondition(20*time.Second, func() bool { return f.leadRunning(t, "posse") && f.leadRunning(t, "clouddbv1") }) {
		t.Fatalf("up --all did not restore the listed Leads: %#v", f.snapshot(t).Panes)
	}
	f.assertLeadStaysGone(t, "after up --all skipped it")
}
