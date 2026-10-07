package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type workerFixture struct {
	root, repo, home, mount string
	project                 store.Project
}

func newWorkerFixture(t *testing.T) workerFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	mount := filepath.Join(home, "remuda", "shop", "mount-1")
	gitTest(t, repo, "worktree", "add", "--detach", mount)
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "ship", Title: "Work", State: store.StateSpawning, LandingMode: "local", PaneID: "w2:p1", WorktreePath: mount})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id,acquired_at,released_at) VALUES(?,?,?,?,?,0,0)`, project.ID, 1, mount, "held", taskID); err != nil {
		t.Fatal(err)
	}
	return workerFixture{root: root, repo: repo, home: home, mount: mount, project: project}
}

func runCLI(t *testing.T, service *Service, args ...string) (int, string) {
	t.Helper()
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out, cli.ErrOut = &output, &output
	return cli.Run(args), output.String()
}

func TestWorkerCannotRunStateChangingCommandsAgainstItsHome(t *testing.T) {
	fixture := newWorkerFixture(t)
	brief := filepath.Join(fixture.root, "brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: T\ndone_when: d\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	forbidden := [][]string{
		{"ride", "--brief", brief},
		{"project", "add", "--name", "stray"},
		{"project", "remove", "shop", "--yes"},
		{"up", "--claude"},
		{"setup", "--check"},
		{"config", "set", "defaults.max_workers", "6"},
		{"unsaddle", "t1"},
		{"roster"},
		{},
	}
	for _, caller := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"pane", func(t *testing.T) {
			t.Setenv("HERDR_ENV", "1")
			t.Setenv("HERDR_PANE_ID", "w2:p1")
			t.Chdir(fixture.repo)
		}},
		{"marker", func(t *testing.T) {
			t.Setenv(workerHomeEnv, fixture.home)
			t.Chdir(fixture.root)
		}},
		{"mount", func(t *testing.T) {
			t.Chdir(filepath.Join(fixture.mount))
		}},
	} {
		t.Run(caller.name, func(t *testing.T) {
			caller.setup(t)
			service := testService(fixture.home, herdr.NewFake())
			for _, args := range forbidden {
				if code, output := runCLI(t, service, args...); code == 0 || !strings.Contains(output, "worker_forbidden") || !strings.Contains(output, "temporary POSSE_HOME") {
					t.Fatalf("Worker (%s) ran `posse %s`: code=%d output=%s", caller.name, strings.Join(args, " "), code, output)
				}
			}
			if code, output := runCLI(t, service, "config", "show"); code != 0 {
				t.Fatalf("Worker (%s) could not read config: code=%d output=%s", caller.name, code, output)
			}
		})
	}
}

func TestWorkerMayExperimentAgainstATemporaryHome(t *testing.T) {
	fixture := newWorkerFixture(t)
	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w2:p1")
	scratch := filepath.Join(fixture.root, "scratch")
	initRepo(t, scratch)
	t.Chdir(scratch)
	service := testService(filepath.Join(fixture.root, "experiment-home"), herdr.NewFake())
	if code, output := runCLI(t, service, "project", "add", "--name", "scratch"); code != 0 || !strings.Contains(output, "registered: true") {
		t.Fatalf("Worker could not register a Project in a temporary home: code=%d output=%s", code, output)
	}
}

func TestWorkerProtocolRequiresIsolatedExperiments(t *testing.T) {
	protocol := workerProtocol(store.Project{Name: "shop"}, store.Task{Seq: 3, Type: "ship", LandingMode: "local"}, dispatch.Brief{DoneWhen: "done"}, "/tmp/launch.md", "/tmp/posse/scratch/shop/t3")
	for _, want := range []string{"# Rider protocol", "isolated Herdr server", "POSSE_HOME under a temp dir", "never touch panes, tabs or workspaces you did not create"} {
		if !strings.Contains(protocol, want) {
			t.Fatalf("Worker protocol lacks %q:\n%s", want, protocol)
		}
	}
}

func TestProjectRemoveUnregistersOnlyEmptyProjects(t *testing.T) {
	fixture := newWorkerFixture(t)
	stray := filepath.Join(fixture.root, "stray")
	initRepo(t, stray)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	strayProject, err := db.CreateProject(context.Background(), "stray", stray, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), strayProject.ID, "w9", "w9:p1", "posse:stray:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	strayHome := filepath.Join(fixture.home, "projects", "stray")
	if err := os.MkdirAll(strayHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(strayHome, "lead.md"), []byte("lead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(fixture.root)
	fake := herdr.NewFake()
	service := testService(fixture.home, fake)

	if code, output := runCLI(t, service, "project", "remove", "shop", "--yes"); code == 0 || !strings.Contains(output, "project_not_empty") {
		t.Fatalf("removed a Project with Tasks: code=%d output=%s", code, output)
	}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w9:p1", Label: "posse:stray:lead", Agent: "claude"}}}
	if code, output := runCLI(t, service, "project", "remove", "stray", "--yes"); code == 0 || !strings.Contains(output, "lead_running") {
		t.Fatalf("removed a Project with a live Lead: code=%d output=%s", code, output)
	}
	fake.SnapshotValue = herdr.Snapshot{}
	if code, output := runCLI(t, service, "project", "remove", "stray"); code != 0 || !strings.Contains(output, "removed: false") {
		t.Fatalf("dry run failed: code=%d output=%s", code, output)
	}
	if _, err := os.Stat(strayHome); err != nil {
		t.Fatalf("dry run deleted the Project home: %v", err)
	}
	if code, output := runCLI(t, service, "project", "remove", "stray", "--yes"); code != 0 || !strings.Contains(output, "removed: true") {
		t.Fatalf("remove failed: code=%d output=%s", code, output)
	}
	if _, err := os.Stat(strayHome); !os.IsNotExist(err) {
		t.Fatalf("Project home survived removal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stray, "README.md")); err != nil {
		t.Fatalf("removal touched the repository: %v", err)
	}
	observer, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if _, err := observer.ProjectByName(context.Background(), "stray"); !store.IsNotFound(err) {
		t.Fatalf("stray Project is still registered: %v", err)
	}
	if _, err := observer.ProjectByName(context.Background(), "shop"); err != nil {
		t.Fatalf("removal touched another Project: %v", err)
	}
}

// fakeProc builds a /proc tree where this process descends from a shell
// (pid 50) that descends from an agent (pid 20).
func fakeProc(t *testing.T, agentCWD string, agentEnv []string) string {
	t.Helper()
	root := t.TempDir()
	write := func(pid, parent int, cwd string, env []string) {
		directory := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "stat"), []byte(fmt.Sprintf("%d (proc name) S %d 0 0", pid, parent)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "environ"), []byte(strings.Join(env, "\x00")+"\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
		if cwd != "" {
			if err := os.Symlink(cwd, filepath.Join(directory, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(os.Getpid(), 50, "/", nil)
	write(50, 20, "/", []string{"PATH=/usr/bin"})
	write(20, 1, agentCWD, agentEnv)
	return root
}

func TestWorkerIdentitySurvivesRemovedEnvironmentAndCwd(t *testing.T) {
	fixture := newWorkerFixture(t)
	brief := filepath.Join(fixture.root, "brief.md")
	t.Chdir(fixture.root)
	t.Setenv(workerHomeEnv, "")
	for _, testCase := range []struct {
		name string
		cwd  string
		env  []string
	}{
		{"marker", "/", []string{workerHomeEnv + "=" + fixture.home}},
		{"mount", filepath.Join(fixture.mount, "internal"), nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Join(fixture.mount, "internal"), 0o700); err != nil {
				t.Fatal(err)
			}
			previous := procRoot
			procRoot = fakeProc(t, testCase.cwd, testCase.env)
			t.Cleanup(func() { procRoot = previous })
			service := testService(fixture.home, herdr.NewFake())
			for _, args := range [][]string{{"config", "set", "defaults.auto_unsaddle", "never"}, {"ride", "--brief", brief}} {
				if code, output := runCLI(t, service, args...); code == 0 || !strings.Contains(output, "worker_forbidden") {
					t.Fatalf("a descendant of a Worker ran `posse %s`: code=%d output=%s", strings.Join(args, " "), code, output)
				}
			}
			if !service.workerCaller(context.Background(), fixture.home) {
				t.Fatal("workerCaller missed the Worker ancestor")
			}
			if service.workerCaller(context.Background(), filepath.Join(fixture.root, "other-home")) {
				t.Fatal("a Worker ancestor made this process a Worker of another home")
			}
		})
	}
	previous := procRoot
	procRoot = fakeProc(t, "/", []string{"PATH=/usr/bin"})
	t.Cleanup(func() { procRoot = previous })
	if testService(fixture.home, herdr.NewFake()).workerCaller(context.Background(), fixture.home) {
		t.Fatal("a process with no Worker ancestor was taken for a Worker")
	}
}

func TestMigrationGateAllowsOnlyTheInstalledBuildOutsideWorkers(t *testing.T) {
	fixture := newWorkerFixture(t)
	path := filepath.Join(fixture.home, "posse.db")
	t.Chdir(fixture.root)
	service := testService(fixture.home, herdr.NewFake())
	if err := service.MigrationGate(path, []int64{13}, false); err != nil {
		t.Fatalf("a home without setup.json refused migration: %v", err)
	}
	if err := writeSetupManifest(filepath.Join(fixture.home, setupManifestName), setupManifest{Version: "1", Binary: "/opt/posse/bin/posse"}); err != nil {
		t.Fatal(err)
	}
	if err := service.MigrationGate(path, []int64{13}, false); err == nil || !strings.Contains(err.Error(), "only the installed posse") {
		t.Fatalf("a foreign build may migrate the home: %v", err)
	}
	if err := service.MigrationGate(path, []int64{13}, true); err != nil {
		t.Fatalf("the User could not force a migration: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSetupManifest(filepath.Join(fixture.home, setupManifestName), setupManifest{Version: "1", Binary: self}); err != nil {
		t.Fatal(err)
	}
	if err := service.MigrationGate(path, []int64{13}, false); err != nil {
		t.Fatalf("the installed build could not migrate: %v", err)
	}
	t.Setenv(workerHomeEnv, fixture.home)
	for _, forced := range []bool{false, true} {
		var schema *store.SchemaError
		if err := service.MigrationGate(path, []int64{13}, forced); !errors.As(err, &schema) || schema.Code != "migration_refused" {
			t.Fatalf("a Worker may migrate its home (forced=%v): %v", forced, err)
		}
	}
}
