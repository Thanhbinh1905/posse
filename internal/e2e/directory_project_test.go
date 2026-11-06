//go:build e2e && linux

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestFolderCLIStartupShapes(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "mixed"}[mixed], func(t *testing.T) {
			f := newWorkerTabsFixtureWithOptions(t, workerTabsOptions{emptyFolder: true, prepareFolder: func(repo string, env []string) {
				if err := os.WriteFile(filepath.Join(repo, "ideas.md"), []byte("plan\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(repo, "plain"), 0o700); err != nil {
					t.Fatal(err)
				}
				if mixed {
					for _, name := range []string{"api", "web"} {
						dir := filepath.Join(repo, name)
						if err := os.Mkdir(dir, 0o700); err != nil {
							t.Fatal(err)
						}
						gitTest(t, env, dir, "init", "-b", "main")
					}
				}
			}})
			kind := "directory"
			if mixed {
				kind = "workspace"
			}
			show := runPosse(t, f.binary, f.repo, f.leadEnv, "project", "show")
			if !strings.Contains(show, "kind: "+kind) {
				t.Fatalf("startup: %s", show)
			}
			runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "lead_running", "up", "--yes")
			if _, err := os.Lstat(filepath.Join(f.repo, ".git")); !os.IsNotExist(err) {
				t.Fatalf("root metadata: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(f.repo, "ideas.md")); err != nil || string(data) != "plan\n" {
				t.Fatalf("source changed: %q %v", data, err)
			}
		})
	}
}

func TestDirectoryProjectCLIRefusalsAndEvolution(t *testing.T) {
	f := newWorkerTabsFixtureWithOptions(t, workerTabsOptions{emptyFolder: true})
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	before, err := db.ProjectByID(context.Background(), f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(f.repo, "ideas.md")
	if err := os.WriteFile(note, []byte("plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(f.root, "explore.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Edit ideas\ndone_when: ideas changed\n---\nEdit ideas.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.root, "alias")
	if err := os.Symlink(f.repo, alias); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(f.repo, "plain")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{f.repo, alias, nested} {
		for _, args := range [][]string{
			{"start", "--brief", brief, "--name", "edit"},
			{"start", "--brief", brief, "--name", "edit", "--queue"},
			{"dispatch", "--brief", brief}, {"sync"}, {"merge", "t1", "--execute"},
		} {
			runPosseExpectingFailure(t, f, cwd, f.leadEnv, "execution_unavailable", args...)
		}
	}
	for _, args := range [][]string{{"publish", "No execution"}, {"review", "start", "t1"}} {
		runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "", args...)
	}
	if output := runPosse(t, f.binary, alias, f.leadEnv, "up", "--all"); !strings.Contains(output, "shop") {
		t.Fatalf("resume: %s", output)
	}
	if output := runPosse(t, f.binary, alias, f.leadEnv, "lead"); !strings.Contains(output, "execution: none") || !strings.Contains(output, "no isolation or rollback") {
		t.Fatalf("lead: %s", output)
	}
	if tasks, err := db.Tasks(context.Background(), f.projectID, true); err != nil || len(tasks) != 0 {
		t.Fatalf("Tasks: %#v %v", tasks, err)
	}
	if _, err := os.Lstat(filepath.Join(f.repo, ".git")); !os.IsNotExist(err) {
		t.Fatalf("Git metadata: %v", err)
	}
	if data, err := os.ReadFile(note); err != nil || string(data) != "plan\n" {
		t.Fatalf("source changed: %q %v", data, err)
	}
	killServer(t, f.server)
	f.server = startServer(t, f.client)
	runPosse(t, f.binary, alias, f.env, "up", "--all")
	lead, found := findLeadInSnapshot(f.snapshot(t))
	if !found {
		t.Fatal("Directory Lead did not restart")
	}
	f.leadPaneID = lead.PaneID
	f.leadEnv = setEnv(f.leadEnv, "HERDR_PANE_ID", lead.PaneID)
	f.leadEnv = setEnv(f.leadEnv, "HERDR_TAB_ID", lead.TabID)
	f.leadEnv = setEnv(f.leadEnv, "HERDR_WORKSPACE_ID", lead.WorkspaceID)
	runPosseExpectingFailure(t, f, alias, f.leadEnv, "execution_unavailable", "start", "--brief", brief)
	for _, name := range []string{"api", "web"} {
		dir := filepath.Join(f.repo, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, dir, "init", "-b", "main")
	}
	if output := runPosse(t, f.binary, alias, f.leadEnv, "project", "scan"); !strings.Contains(output, "converted: true") || !strings.Contains(output, "kind: workspace") {
		t.Fatalf("scan: %s", output)
	}
	after, err := db.ProjectByID(context.Background(), f.projectID)
	if err != nil || after.UUID != before.UUID || after.Root != before.Root || after.Name != before.Name {
		t.Fatalf("identity: %#v %v", after, err)
	}
	members, err := db.ProjectRepos(context.Background(), f.projectID)
	if err != nil || len(members) != 2 || members[0].Name != "api" || members[1].Name != "web" {
		t.Fatalf("members: %#v %v", members, err)
	}
	if projects, err := db.Projects(context.Background()); err != nil || len(projects) != 1 {
		t.Fatalf("Projects: %#v %v", projects, err)
	}
}
