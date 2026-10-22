package app

import (
	"bufio"
	"context"
	"github.com/thanhbinh1905/posse/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWatchProcessListingMatchesPaneAndPosseHome(t *testing.T) {
	listing := "12 /usr/local/bin/posse watch --poll-only HERDR_PANE_ID=w1:p9 POSSE_HOME=/tmp/other HOME=/tmp\n" +
		"13 /usr/local/bin/posse watch --poll-only HERDR_PANE_ID=w1:p2 POSSE_HOME=/tmp/shop HOME=/tmp\n"
	if !watchProcessInListingWithVerifier(listing, "w1:p2", "/tmp/shop", func(path string) bool { return filepath.Base(path) == "posse" }) {
		t.Fatal("Darwin process listing missed matching Watch")
	}
	if watchProcessInListingWithVerifier(listing, "w1:p9", "/tmp/shop", func(path string) bool { return filepath.Base(path) == "posse" }) {
		t.Fatal("mixed separate Watch homes")
	}
}

func TestWatchProcessListingRecognizesLeadWatchersRenamedBinariesAndDefaultHome(t *testing.T) {
	listing := "21 /nix/store/posse/bin/posse watch --json --quiet-routine HERDR_PANE_ID=w1:p1 POSSE_HOME=/tmp/shop HOME=/tmp PWD=/tmp/shop\n" +
		"22 /usr/local/bin/posse watch --ack 9 HOME=/home/user POSSE_HOME=/home/user/.posse PWD=/repo\n" +
		"23 /usr/local/bin/renamed-posse watch --poll-only POSSE_HOME=/tmp/shop HOME=/tmp\n" +
		"24 /usr/local/bin/unrelated watch --poll-only POSSE_HOME=/tmp/shop HOME=/tmp\n"
	isPosse := func(path string) bool {
		name := filepath.Base(path)
		return name == "posse" || name == "renamed-posse"
	}
	processes := watchProcessesInListingWithVerifier(listing, "/tmp/shop", isPosse)
	if len(processes) != 2 {
		t.Fatalf("found %d watches, want the matching-home Lead and renamed poll-only binary: %#v", len(processes), processes)
	}
	if processes[0].PID != 21 || processes[0].Kind != "lead" || !processes[0].QuietRoutine || processes[0].PaneID != "w1:p1" {
		t.Fatalf("Lead watcher = %#v", processes[0])
	}
	if processes[1].PID != 23 || processes[1].Kind != "watch" || !processes[1].PollOnly {
		t.Fatalf("renamed poll-only watcher = %#v", processes[1])
	}
	if _, found := watchFromArgs(25, []string{"/bin/sh", "-c", "posse watch --poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop"}, "/tmp/shop"); found {
		t.Fatal("matched a shell whose command text contains a Watch invocation")
	}
	if _, found := watchFromArgs(26, []string{"/tmp/renamed-posse", "watch", "--poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop"}, "/tmp/shop"); !found {
		t.Fatal("missed the renamed binary's CLI argument pattern")
	}
	if hook, found := watchFromArgs(27, []string{"/usr/local/bin/posse", "_claude-stop"}, map[string]string{"POSSE_HOME": "/tmp/shop", "HERDR_PANE_ID": "w1:p1"}, "/tmp/shop"); !found || hook.Kind != "lead" || hook.PollOnly {
		t.Fatalf("Claude Stop hook is not discoverable for fallback, down and update: %#v %v", hook, found)
	}
	if got := processHome(map[string]string{"HOME": "/home/user"}); got != "/home/user/.posse" {
		t.Fatalf("default process home = %q", got)
	}
	defaultHome := watchProcessesInListingWithVerifier("24 /usr/local/bin/posse watch HOME=/home/user PWD=/repo\n", "/home/user/.posse", func(path string) bool { return filepath.Base(path) == "posse" })
	if len(defaultHome) != 1 || defaultHome[0].Kind != "lead" {
		t.Fatalf("default-home Lead watcher = %#v", defaultHome)
	}
}

func TestWatchProjectDiscoveryMatchesNestedWorkingDirectory(t *testing.T) {
	project := store.Project{Name: "shop", Root: "/workspace/shop", LeadPaneID: "w1:p1"}
	if !processBelongsToProject(watchProcess{WorkingDir: "/workspace/shop/tools"}, project) {
		t.Fatal("watch from a Project subdirectory was not matched")
	}
	if processBelongsToProject(watchProcess{WorkingDir: "/workspace/shopping"}, project) {
		t.Fatal("watch from a sibling path was matched")
	}
}

func TestSupervisedWatchDiscoveryExcludesForegroundPollOnlyRuns(t *testing.T) {
	project := store.Project{ID: 42, Root: "/workspace/shop"}
	processes := []watchProcess{
		{PID: 11, PollOnly: true, WorkingDir: project.Root},
		{PID: 12, PollOnly: true, WatchProjectID: project.ID, WatchStartedAt: 100},
		{PID: 13, PollOnly: true, WatchProjectID: project.ID, WatchToken: "managed-token", WatchStartedAt: 100},
	}
	got := supervisedWatchProcessesForProject(processes, project)
	if len(got) != 1 || got[0].PID != 13 {
		t.Fatalf("supervised Project Watches = %#v, want only the process with a complete private identity", got)
	}
}

func TestLiveLeadWatchExcludesPollOnlyAndOtherProjects(t *testing.T) {
	project := store.Project{Root: "/workspace/shop", HerdrWorkspaceID: "w1", LeadPaneID: "w1:p1"}
	pid := os.Getpid()
	if liveLeadWatch(project, []watchProcess{{PID: pid, PaneID: "w1:p2", WorkingDir: project.Root, PollOnly: true}}) {
		t.Fatal("poll-only Project process is not the Lead delivery watcher")
	}
	if liveLeadWatch(project, []watchProcess{{PID: pid, PaneID: "w1:p4", WorkingDir: "/workspace/other"}}) {
		t.Fatal("other Project's watcher in the same workspace is not this Lead's watcher")
	}
	if !liveLeadWatch(project, []watchProcess{{PID: pid, PaneID: "w1:p1", WorkingDir: project.Root}}) {
		t.Fatal("live Lead watcher was not detected")
	}
	if liveLeadWatch(project, []watchProcess{{PID: -1, PaneID: "w1:p1", WorkingDir: project.Root}}) {
		t.Fatal("dead watcher blocked the typed fallback")
	}
}

func TestStopWatchesRefusesUnverifiedExecutableIdentity(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "trap '' TERM; printf 'ready\\n'; while :; do :; done")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("process readiness = %q, %v", line, err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	_, err = stopWatchProcesses(context.Background(), t.TempDir(), []watchProcess{{PID: command.Process.Pid, Project: "shop", Kind: "lead"}})
	if err == nil {
		t.Fatal("stopWatchProcesses accepted a process without verified executable identity")
	}
	if !watchPIDRunning(command.Process.Pid) {
		t.Fatalf("refusing the unverified process still terminated PID %d", command.Process.Pid)
	}
}

func TestStopWatchesIgnoresProcessThatExitedBeforeStop(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if !watchPIDRunning(pid) {
		t.Fatal("test process did not start")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait() // The killed process exits with a signal status.

	_, err := stopWatchProcesses(context.Background(), t.TempDir(), []watchProcess{{
		PID:        pid,
		Executable: watchExecutableIdentity{Path: "/proc/exited/exe", Inode: 1},
		Project:    "shop",
		Kind:       "lead",
	}})
	if err != nil {
		t.Fatalf("stopping an already exited Watch: %v", err)
	}
}

func TestStopWatchesEscalatesToSIGKILL(t *testing.T) {
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	previousGracePeriod := watchStopGracePeriod
	watchStopGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { watchStopGracePeriod = previousGracePeriod })
	command := exec.Command("/bin/sh", "-c", "trap '' TERM; printf 'ready\\n'; while :; do :; done")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("SIGTERM-ignoring process readiness = %q, %v", line, err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	executable, ok := inspectWatchExecutable(command.Process.Pid)
	if !ok {
		t.Fatal("could not inspect the test process executable")
	}
	processes, err := stopWatchProcesses(context.Background(), t.TempDir(), []watchProcess{{PID: command.Process.Pid, Executable: executable, Project: "shop", Kind: "lead"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 1 || !processes[0].ForceKilled || watchPIDRunning(command.Process.Pid) {
		t.Fatalf("SIGTERM-ignoring process was not killed: %#v", processes)
	}
}

func TestWatchRuntimeEnvironmentIsDetachedFromHerdrIdentity(t *testing.T) {
	project := store.Project{ID: 42, Name: "shop", Root: "/workspace/shop"}
	env := watchRuntimeEnvironment([]string{
		"PATH=/usr/bin", "POSSE_HOME=/tmp/old", "POSSE_TASK_ID=t7", "POSSE_TASK_TEMP=/tmp/task", "POSSE_INTENT_CRASH_AT=merge", "POSSE_TEST_EXTERNAL_COMMAND_TIMEOUT=60s", "GIT_DIR=/repo/.git", "GIT_WORK_TREE=/repo", "GIT_INDEX_FILE=/repo/.git/index", "GIT_SSH_COMMAND=ssh -i /home/user/key", "HERDR_PANE_ID=w1:p4", "HERDR_WORKSPACE_ID=w1", "HERDR_TAB_ID=w1:t1", "HERDR_ENV=1", "HERDR_SOCKET_PATH=/named/session/herdr.sock", "HERDR_SESSION=dev", "HERDR_BIN_PATH=/usr/local/bin/herdr", "HERDR_CONFIG_PATH=/home/user/herdr.toml", "HERDR_PLUGIN_EVENT=focus", "CUSTOM=value",
	}, "/tmp/posse", project, "random-token", 1234)
	got := parseProcessEnvironment(strings.Join(env, "\x00"))
	if got["POSSE_HOME"] != "/tmp/posse" || got[watchRuntimeProjectEnv] != "42" || got[watchRuntimeTokenEnv] != "random-token" || got[watchRuntimeStartedEnv] != "1234" || got["PWD"] != project.Root {
		t.Fatalf("watch identity environment = %#v", got)
	}
	for _, key := range []string{"HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID", "HERDR_PLUGIN_EVENT", "POSSE_TASK_ID", "POSSE_TASK_TEMP", "POSSE_INTENT_CRASH_AT", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		if _, found := got[key]; found {
			t.Errorf("detached watch inherited %s", key)
		}
	}
	for key, want := range map[string]string{
		"HERDR_ENV": "1", "HERDR_SOCKET_PATH": "/named/session/herdr.sock", "HERDR_SESSION": "dev", "HERDR_BIN_PATH": "/usr/local/bin/herdr", "HERDR_CONFIG_PATH": "/home/user/herdr.toml", "GIT_SSH_COMMAND": "ssh -i /home/user/key",
	} {
		if got[key] != want {
			t.Errorf("watch lost environment setting %s: got %q, want %q", key, got[key], want)
		}
	}
	if got["CUSTOM"] != "value" || got["POSSE_TEST_EXTERNAL_COMMAND_TIMEOUT"] != "60s" {
		t.Fatalf("watch lost unrelated or test-harness environment: %#v", got)
	}
}

func TestWatchRuntimeRotatesLargeLogBeforeStarting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	previousLimit := watchRuntimeLogMaxBytes
	watchRuntimeLogMaxBytes = 10
	t.Cleanup(func() { watchRuntimeLogMaxBytes = previousLimit })
	if err := rotateWatchRuntimeLog(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rotated Watch log still exists at its active path: %v", err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil || string(backup) != strings.Repeat("x", 20) {
		t.Fatalf("rotated Watch log = %q, %v", backup, err)
	}
}

func TestWatchRuntimeLogRotatesWhileThePollerIsRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.log")
	previousLimit := watchRuntimeLogMaxBytes
	watchRuntimeLogMaxBytes = 10
	t.Cleanup(func() { watchRuntimeLogMaxBytes = previousLimit })
	writer, err := openWatchRuntimeLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("abcdefghijklmnop")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != "klmnop" || string(backup) != "abcdefghij" {
		t.Fatalf("active and rotated Watch logs = %q and %q", active, backup)
	}
}

func TestStartedWatchProcessIsReapedByItsParent(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 0")
	if err := startWatchProcess(command); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(watchProcRoot, strconv.Itoa(command.Process.Pid))); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("exited child PID %d remained in the process table; Wait did not reap it", command.Process.Pid)
}

func TestUnboundIndefinitePollOnlyWatchIsMarkedForReaping(t *testing.T) {
	project := store.Project{ID: 42, Root: "/workspace/shop"}
	unbounded, found := watchFromArgs(31, []string{"/usr/local/bin/posse", "watch", "--poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop", "PWD": project.Root}, "/tmp/shop")
	if !found || !unbounded.PollOnly || unbounded.PollOnlyBounded {
		t.Fatalf("unbounded poll-only Watch = %#v, found=%t", unbounded, found)
	}
	bounded, found := watchFromArgs(32, []string{"/usr/local/bin/posse", "watch", "--poll-only", "--timeout", "2500"}, map[string]string{"POSSE_HOME": "/tmp/shop", "PWD": project.Root}, "/tmp/shop")
	if !found || !bounded.PollOnly || !bounded.PollOnlyBounded {
		t.Fatalf("bounded poll-only Watch = %#v, found=%t", bounded, found)
	}
	leaked := leakedPollOnlyWatchesForProject([]watchProcess{unbounded, bounded}, project)
	if len(leaked) != 1 || leaked[0].PID != unbounded.PID {
		t.Fatalf("leaked poll-only Watches = %#v, want only unbounded process", leaked)
	}
}

func TestWatchProcessListingExtractsWatchIdentity(t *testing.T) {
	listing := "31 /usr/local/bin/posse watch --poll-only POSSE_HOME=/tmp/shop POSSE_WATCH_PROJECT_ID=8 POSSE_WATCH_TOKEN=abc POSSE_WATCH_STARTED_AT=900 PWD=/workspace/shop\n"
	processes := watchProcessesInListingWithVerifier(listing, "/tmp/shop", func(path string) bool { return filepath.Base(path) == "posse" })
	if len(processes) != 1 {
		t.Fatalf("watch processes = %#v", processes)
	}
	process := processes[0]
	if process.Kind != "watch" || !process.PollOnly || process.WatchProjectID != 8 || process.WatchToken != "abc" || process.WatchStartedAt != 900 || process.WorkingDir != "/workspace/shop" {
		t.Fatalf("watch identity = %#v", process)
	}
}

func TestWatchRuntimeHealthReportsMissingProcess(t *testing.T) {
	ctx := context.Background()
	db, project, home := newWatchTestProject(t)
	defer db.Close()
	project.HerdrWorkspaceID = "w1"
	service := testService(home, nil)
	check, err := service.watchRuntimeHealth(ctx, db, home, project)
	if err != nil {
		t.Fatal(err)
	}
	if check.Status != "warn" || !strings.Contains(check.Detail, "watch=missing") {
		t.Fatalf("missing watch health = %#v", check)
	}
}
