package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

func TestSystemdWatchCanBeDisabledInTestEnvironment(t *testing.T) {
	t.Setenv("POSSE_TEST_HERDR", "1")
	t.Setenv("POSSE_TEST_DISABLE_SYSTEMD_WATCH", "1")
	if systemdUserManagerAvailable(context.Background()) {
		t.Fatal("systemd watch supervision is available while disabled for tests")
	}
}

func TestSystemdWatchStopsActiveUnitWhenStoredPIDExited(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd user units are Linux-only")
	}
	directory := t.TempDir()
	stopped := filepath.Join(directory, "stopped")
	script := "#!/bin/sh\ncase \"$*\" in *LoadState*) printf loaded;; *\" stop \"*) printf stopped > \"$STOP_MARKER\";; *) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(directory, "systemctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("STOP_MARKER", stopped)
	err := stopStoredWatchRuntimeForDown(context.Background(), directory, watchProcess{PID: 99999999, WatchProjectID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(stopped); err != nil || string(contents) != "stopped" {
		t.Fatalf("active systemd unit was not stopped after stored PID exited: %q, %v", contents, err)
	}
}

func TestSystemdWatchStartKeepsReplacementRuntimeRow(t *testing.T) {
	if runtime.GOOS != "linux" || !systemdUserManagerAvailable(context.Background()) {
		t.Skip("requires a Linux systemd user manager")
	}
	root, err := os.MkdirTemp(os.Getenv("TMPDIR"), "posse-systemd-start-race-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	fixture := newWorkerWatchFixture(t, root)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByID(context.Background(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	previousStarter := startSystemdWatchUnit
	startSystemdWatchUnit = func(_ context.Context, _ string, project store.Project, binary, token string, startedAt int64, _ string) error {
		identity, ok := watchExecutableAt(binary)
		if !ok {
			return errors.New("test binary identity unavailable")
		}
		replacement := store.WatchRuntime{PID: 34568, Token: token, StartedAt: startedAt, HeartbeatAt: startedAt, ExecutablePath: identity.Path, ExecutableDevice: identity.Device, ExecutableInode: identity.Inode}
		if err := db.SetWatchRuntime(context.Background(), project.ID, replacement); err != nil {
			return err
		}
		return nil
	}
	t.Cleanup(func() { startSystemdWatchUnit = previousStarter })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	service := New(fixture.home, nil)
	if err := service.startWatchRuntime(context.Background(), db, fixture.home, project, binary); err != nil {
		t.Fatal(err)
	}
	got, err := db.WatchRuntime(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 34568 {
		t.Fatalf("systemd replacement runtime PID = %d, want 34568", got.PID)
	}
}

func TestSystemdWatchScopeLauncherHelper(t *testing.T) {
	if os.Getenv("POSSE_TEST_WATCH_SCOPE_HELPER") != "1" {
		return
	}
	home := os.Getenv("POSSE_TEST_WATCH_HOME")
	projectID, err := strconv.ParseInt(os.Getenv("POSSE_TEST_WATCH_PROJECT_ID"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("POSSE_TEST_WATCH_BINARY")
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	defer func() { watchExecutableVerifier = previousVerifier }()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(home, nil).startWatchRuntime(context.Background(), db, home, project, binary); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestSystemdWatchSurvivesLaunchingScopeTermination(t *testing.T) {
	if runtime.GOOS != "linux" || !systemdUserManagerAvailable(context.Background()) {
		t.Skip("requires a Linux systemd user manager")
	}
	root, err := os.MkdirTemp(os.Getenv("TMPDIR"), "posse-watch-scope-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	fixture := newWorkerWatchFixture(t, root)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	binary := filepath.Join(root, "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	build.Dir = filepath.Clean(filepath.Join(workingDir, "../.."))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Posse CLI: %v\n%s", err, output)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	scope := fmt.Sprintf("posse-watch-launch-%d.scope", os.Getpid())
	args := []string{"--user", "--scope", "--unit=" + scope, "--setenv=POSSE_TEST_WATCH_SCOPE_HELPER=1", "--setenv=POSSE_TEST_WATCH_HOME=" + fixture.home, "--setenv=POSSE_TEST_WATCH_PROJECT_ID=" + strconv.FormatInt(fixture.project.ID, 10), "--setenv=POSSE_TEST_WATCH_BINARY=" + binary, "--setenv=POSSE_HOME=" + fixture.home, "--working-directory=" + fixture.repo, "--", testBinary, "-test.run=^TestSystemdWatchScopeLauncherHelper$"}
	launcher := exec.Command("systemd-run", args...)
	var launcherOutput bytes.Buffer
	launcher.Stdout, launcher.Stderr = &launcherOutput, &launcherOutput
	if err := launcher.Start(); err != nil {
		t.Fatal(err)
	}
	watch, err := waitForWatchRuntime(context.Background(), db, fixture.project.ID)
	if err != nil {
		_ = exec.Command("systemctl", "--user", "kill", "--kill-whom=all", "--signal=SIGKILL", scope).Run()
		_ = launcher.Wait()
		t.Fatalf("wait for Project Watch started in launching scope: %v; launcher output: %s", err, launcherOutput.String())
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "kill", "--kill-whom=all", "--signal=SIGKILL", scope).Run()
		_ = stopSystemdWatch(context.Background(), fixture.home, fixture.project.ID)
		_ = db.ClearWatchRuntime(context.Background(), fixture.project.ID, watch.PID, watch.Token)
	})
	if output, err := exec.Command("systemctl", "--user", "kill", "--kill-whom=all", "--signal=SIGKILL", scope).CombinedOutput(); err != nil {
		_ = launcher.Process.Kill()
		_ = launcher.Wait()
		t.Fatalf("kill launching scope %s: %v: %s", scope, err, output)
	}
	_ = launcher.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && watchPIDRunning(watch.PID) {
		time.Sleep(50 * time.Millisecond)
	}
	if !watchPIDRunning(watch.PID) || !systemdWatchUnitActive(context.Background(), fixture.home, fixture.project.ID) {
		t.Fatalf("Project Watch did not survive termination of launching scope %s: %#v", scope, watch)
	}
}

func waitForWatchRuntime(ctx context.Context, db *store.DB, projectID int64) (store.WatchRuntime, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		watch, err := db.WatchRuntime(ctx, projectID)
		if err != nil {
			return store.WatchRuntime{}, err
		}
		if watch.PID > 1 && watch.Token != "" && watchPIDRunning(watch.PID) {
			return watch, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return store.WatchRuntime{}, errors.New("systemd Project Watch did not register before timeout")
}

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
