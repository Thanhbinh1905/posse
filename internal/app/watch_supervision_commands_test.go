package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

const (
	watchFixtureWorkspaceID  = "w1"
	watchFixtureLeadPaneID   = "w1:p1"
	watchFixtureLeadTabID    = "w1:t1"
	watchFixtureWorkerPaneID = "w2:p1"
	watchFixtureWorkerTabID  = "w2:t1"
)

func TestSystemdWatchUnitNameIsStableAndHomeScoped(t *testing.T) {
	first := systemdWatchUnitName("/home/user/.posse", 42)
	if first == "" || first != systemdWatchUnitName("/home/user/.posse", 42) {
		t.Fatalf("unit name is not stable: %q", first)
	}
	if first == systemdWatchUnitName("/home/user/other", 42) || first == systemdWatchUnitName("/home/user/.posse", 43) {
		t.Fatalf("unit name does not distinguish home and Project: %q", first)
	}
}

func TestWorkerCommandsSkipWatchSupervision(t *testing.T) {
	fixture := newWorkerFixture(t)
	t.Setenv(workerHomeEnv, fixture.home)
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(fixture.home, nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	for _, name := range []string{"validate", "publish", "signal"} {
		command := &axi.Command{Name: name, Handler: func(*axi.Context, []string) error { return nil }}
		service.structureStoreErrors(command)
		if err := command.Handler(ctx, nil); err != nil {
			t.Fatalf("Worker %s command failed: %v", name, err)
		}
	}
	if calls != 0 {
		t.Fatalf("Worker commands triggered Project Watch supervision %d times", calls)
	}
}

func newWorkerWatchFixture(t *testing.T, root string) workerFixture {
	t.Helper()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	sandbox := filepath.Join(home, "pool", "shop", "sandbox-1")
	gitTest(t, repo, "worktree", "add", "--detach", sandbox)
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
	if err := db.SetProjectLead(ctx, project.ID, watchFixtureWorkspaceID, watchFixtureLeadPaneID, "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: 1, Type: "change", Title: "Work", State: store.StateSpawning, MergeMode: "local", PaneID: watchFixtureWorkerPaneID, WorktreePath: sandbox})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id,acquired_at,released_at) VALUES(?,?,?,?,?,0,0)`, project.ID, 1, sandbox, "held", taskID); err != nil {
		t.Fatal(err)
	}
	return workerFixture{root: root, repo: repo, home: home, sandbox: sandbox, project: project}
}

func TestWorkerSignalPreservesRunningProjectWatch(t *testing.T) {
	root, err := os.MkdirTemp(os.Getenv("TMPDIR"), "posse-e2e-t847-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	fixture := newWorkerWatchFixture(t, root)
	env := herdr.IsolatedTestEnvironment(root)
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(key, "HERDR_") {
			t.Setenv(key, "")
		}
	}
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if found {
			t.Setenv(key, value)
		}
	}
	if _, err := herdr.WriteIsolatedConfig(fixture.root); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByID(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(workingDir, "../.."))
	binary := filepath.Join(fixture.root, "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Posse CLI: %v\n%s", err, output)
	}
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previousStarter := startWatchProcess
	testBinaryStarted := false
	startWatchProcess = func(command *exec.Cmd) error {
		if filepath.Clean(command.Path) == filepath.Clean(testBinary) {
			testBinaryStarted = true
			return errors.New("test guard refused to start the Go test binary as a Watch")
		}
		return previousStarter(command)
	}
	t.Cleanup(func() { startWatchProcess = previousStarter })
	service := New(fixture.home, herdr.NewFake())
	if err := service.startWatchRuntime(ctx, db, fixture.home, project, binary); err != nil {
		t.Fatal(err)
	}
	before, err := db.WatchRuntime(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	watchDeadline := time.Now().Add(5 * time.Second)
	for !watchPIDRunning(before.PID) && time.Now().Before(watchDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !watchPIDRunning(before.PID) {
		t.Fatalf("Project Watch PID %d did not start", before.PID)
	}
	if systemdUserManagerAvailable(ctx) {
		unit := systemdWatchUnitName(fixture.home, project.ID)
		if !systemdWatchUnitActive(ctx, fixture.home, project.ID) {
			t.Fatalf("Project Watch systemd unit %s is not active", unit)
		}
		properties, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=Restart,MemoryMax,TasksMax", unit).CombinedOutput()
		if err != nil || !strings.Contains(string(properties), "Restart=always") || !strings.Contains(string(properties), "MemoryMax=2147483648") || !strings.Contains(string(properties), "TasksMax=256") {
			t.Fatalf("Project Watch unit properties = %s, err=%v", properties, err)
		}
		cgroup, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=ControlGroup", "--value", unit).Output()
		if err != nil || !strings.HasSuffix(strings.TrimSpace(string(cgroup)), ".service") {
			t.Fatalf("Project Watch cgroup = %q, err=%v; want a service outside the launching scope", cgroup, err)
		}
		process, err := os.FindProcess(before.PID)
		if err != nil || process.Kill() != nil {
			t.Fatalf("kill Project Watch PID %d to exercise systemd restart: %v", before.PID, err)
		}
		restartDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(restartDeadline) {
			before, err = db.WatchRuntime(ctx, project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.PID > 1 && before.PID != process.Pid && watchPIDRunning(before.PID) && systemdWatchUnitActive(ctx, fixture.home, project.ID) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if before.PID <= 1 || before.PID == process.Pid || !watchPIDRunning(before.PID) {
			t.Fatalf("systemd did not restart Project Watch after crash: %#v", before)
		}
	}
	var watchToStop watchProcess
	found := false
	watchDeadline = time.Now().Add(5 * time.Second)
	for !found && time.Now().Before(watchDeadline) {
		watchToStop, found = inspectWatchProcess(fixture.home, before.PID)
		if !found {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !found {
		t.Fatalf("Project Watch PID %d was not discoverable", before.PID)
	}
	t.Cleanup(func() {
		if err := stopStoredWatchRuntime(context.Background(), fixture.home, watchToStop); err != nil {
			t.Errorf("stop Project Watch PID %d: %v", before.PID, err)
		}
	})
	heartbeatDeadline := time.Now().Add(10 * time.Second)
	lastHeartbeat := before.HeartbeatAt
	heartbeatAdvanced := false
	stableSince := time.Now()
	for time.Now().Before(heartbeatDeadline) {
		current, err := db.WatchRuntime(ctx, project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.HeartbeatAt != lastHeartbeat {
			lastHeartbeat = current.HeartbeatAt
			stableSince = time.Now()
		}
		if current.HeartbeatAt > before.HeartbeatAt {
			heartbeatAdvanced = true
		}
		if heartbeatAdvanced && time.Since(stableSince) >= 150*time.Millisecond {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !heartbeatAdvanced || time.Since(stableSince) < 150*time.Millisecond {
		t.Fatal("Project Watch did not settle after its initial heartbeat")
	}
	before.HeartbeatAt = time.Now().Add(-time.Minute).UnixMilli()
	if err := db.SetWatchRuntime(ctx, project.ID, before); err != nil {
		t.Fatal(err)
	}
	oldHeartbeatInterval, oldStaleAfter := watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter
	watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter = time.Millisecond, time.Millisecond
	t.Cleanup(func() { watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter = oldHeartbeatInterval, oldStaleAfter })
	t.Setenv(workerHomeEnv, fixture.home)
	t.Setenv("HERDR_PANE_ID", task.PaneID)
	t.Chdir(fixture.sandbox)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		Workspaces: []herdr.Workspace{{WorkspaceID: watchFixtureWorkspaceID, Root: fixture.repo}},
		Tabs:       []herdr.Tab{{TabID: watchFixtureLeadTabID, WorkspaceID: watchFixtureWorkspaceID}, {TabID: watchFixtureWorkerTabID, WorkspaceID: watchFixtureWorkspaceID}},
		Panes: []herdr.Pane{
			{PaneID: watchFixtureLeadPaneID, WorkspaceID: watchFixtureWorkspaceID, TabID: watchFixtureLeadTabID, CWD: fixture.repo, Agent: "pi", AgentStatus: "working"},
			{PaneID: task.PaneID, WorkspaceID: watchFixtureWorkspaceID, TabID: watchFixtureWorkerTabID, CWD: fixture.sandbox, Agent: "pi", AgentStatus: "working"},
		},
	}
	service = New(fixture.home, fake)
	for _, args := range [][]string{{"signal", "working", "Watch regression check"}, {"validate", "status"}} {
		code, output := runCLI(t, service, args...)
		if strings.Contains(output, "worker_forbidden") {
			t.Fatalf("Worker command %q attempted forbidden watch supervision: %s", args, output)
		}
		if args[0] == "signal" && code != 0 || args[0] == "validate" && (code == 0 || !strings.Contains(output, "validation_not_selected")) {
			t.Fatalf("Worker command %q returned unexpected result: exit=%d output=%s", args, code, output)
		}
	}
	if testBinaryStarted {
		t.Fatal("Worker command attempted to start the Go test binary as a Project Watch")
	}
	after, err := db.WatchRuntime(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PID != before.PID || after.Token != before.Token || !watchPIDRunning(before.PID) {
		t.Fatalf("Worker signal replaced the Project Watch: before=%#v after=%#v", before, after)
	}
	log, err := os.ReadFile(watchRuntimeLogPath(fixture.home, project))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "worker_forbidden") {
		t.Fatalf("Project Watch log contains a Worker restart refusal: %s", log)
	}
}

func TestNoticeOnlyCommandsSkipWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	for _, test := range []struct {
		command string
		args    []string
	}{{command: "ack"}, {command: "lookout"}, {command: "watch", args: []string{"--receipt=receipt"}}, {command: "watch", args: []string{"--handoff", "--destination", "pi:session"}}, {command: "watch", args: []string{"--handoff", "--destination", "opencode:session"}}} {
		name := test.command + " " + strings.Join(test.args, " ")
		t.Run(name, func(t *testing.T) {
			before := calls
			service.superviseWatchRuntimes(ctx, test.command, test.args)
			if calls != before {
				t.Fatalf("delivery-only %s command probed Project Watch supervision", name)
			}
		})
	}
}

func TestForegroundPollOnlyWatchSkipsWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	service.superviseWatchRuntimes(ctx, "watch", []string{"--poll-only", "--timeout", "2500"})
	if calls != 0 {
		t.Fatal("foreground --poll-only Watch was supervised after its wait loop")
	}
}

func TestWatchRepairSkipsDuplicateWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	service.superviseWatchRuntimes(ctx, "watch", []string{"--repair"})
	if calls != 0 {
		t.Fatal("Watch repair redundantly supervised the runtime it just repaired")
	}
}

func TestWatchCommandRunsWatchSupervision(t *testing.T) {
	previous := watchExecutableVerifier
	t.Cleanup(func() { watchExecutableVerifier = previous })
	calls := 0
	watchExecutableVerifier = func(string) bool {
		calls++
		return false
	}
	service := New(t.TempDir(), nil)
	ctx := &axi.Context{Context: context.Background(), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}
	service.superviseWatchRuntimes(ctx, "watch", nil)
	if calls == 0 {
		t.Fatal("Watch command did not run Project Watch supervision")
	}
}
