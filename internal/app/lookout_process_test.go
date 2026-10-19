package app

import (
	"bufio"
	"context"
	"github.com/thanhbinh1905/posse/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLookoutProcessListingMatchesPaneAndPosseHome(t *testing.T) {
	listing := "12 /usr/local/bin/posse lookout --poll-only HERDR_PANE_ID=w1:p9 POSSE_HOME=/tmp/other HOME=/tmp\n" +
		"13 /usr/local/bin/posse lookout --poll-only HERDR_PANE_ID=w1:p2 POSSE_HOME=/tmp/shop HOME=/tmp\n"
	if !lookoutProcessInListingWithVerifier(listing, "w1:p2", "/tmp/shop", func(path string) bool { return filepath.Base(path) == "posse" }) {
		t.Fatal("Darwin process listing missed matching Lookout")
	}
	if lookoutProcessInListingWithVerifier(listing, "w1:p9", "/tmp/shop", func(path string) bool { return filepath.Base(path) == "posse" }) {
		t.Fatal("mixed separate Lookout homes")
	}
}

func TestLookoutProcessListingRecognizesLeadWatchersRenamedBinariesAndDefaultHome(t *testing.T) {
	listing := "21 /nix/store/posse/bin/posse lookout --json --quiet-routine HERDR_PANE_ID=w1:p1 POSSE_HOME=/tmp/shop HOME=/tmp PWD=/tmp/shop\n" +
		"22 /usr/local/bin/posse lookout --ack 9 HOME=/home/user POSSE_HOME=/home/user/.posse PWD=/repo\n" +
		"23 /usr/local/bin/renamed-posse lookout --poll-only POSSE_HOME=/tmp/shop HOME=/tmp\n" +
		"24 /usr/local/bin/unrelated lookout --poll-only POSSE_HOME=/tmp/shop HOME=/tmp\n"
	isPosse := func(path string) bool {
		name := filepath.Base(path)
		return name == "posse" || name == "renamed-posse"
	}
	processes := lookoutProcessesInListingWithVerifier(listing, "/tmp/shop", isPosse)
	if len(processes) != 2 {
		t.Fatalf("found %d lookouts, want the matching-home Lead and renamed poll-only binary: %#v", len(processes), processes)
	}
	if processes[0].PID != 21 || processes[0].Kind != "lead" || !processes[0].QuietRoutine || processes[0].PaneID != "w1:p1" {
		t.Fatalf("Lead watcher = %#v", processes[0])
	}
	if processes[1].PID != 23 || processes[1].Kind != "poller" || !processes[1].PollOnly {
		t.Fatalf("renamed poll-only watcher = %#v", processes[1])
	}
	if _, found := lookoutFromArgs(25, []string{"/bin/sh", "-c", "posse lookout --poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop"}, "/tmp/shop"); found {
		t.Fatal("matched a shell whose command text contains a Lookout invocation")
	}
	if _, found := lookoutFromArgs(26, []string{"/tmp/renamed-posse", "lookout", "--poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop"}, "/tmp/shop"); !found {
		t.Fatal("missed the renamed binary's CLI argument pattern")
	}
	if hook, found := lookoutFromArgs(27, []string{"/usr/local/bin/posse", "_claude-stop"}, map[string]string{"POSSE_HOME": "/tmp/shop", "HERDR_PANE_ID": "w1:p1"}, "/tmp/shop"); !found || hook.Kind != "lead" || hook.PollOnly {
		t.Fatalf("Claude Stop hook is not discoverable for fallback, down and update: %#v %v", hook, found)
	}
	if got := processHome(map[string]string{"HOME": "/home/user"}); got != "/home/user/.posse" {
		t.Fatalf("default process home = %q", got)
	}
	defaultHome := lookoutProcessesInListingWithVerifier("24 /usr/local/bin/posse lookout HOME=/home/user PWD=/repo\n", "/home/user/.posse", func(path string) bool { return filepath.Base(path) == "posse" })
	if len(defaultHome) != 1 || defaultHome[0].Kind != "lead" {
		t.Fatalf("default-home Lead watcher = %#v", defaultHome)
	}
}

func TestLookoutProjectDiscoveryMatchesNestedWorkingDirectory(t *testing.T) {
	project := store.Project{Name: "shop", Root: "/workspace/shop", LeadPaneID: "w1:p1"}
	if !processBelongsToProject(lookoutProcess{WorkingDir: "/workspace/shop/tools"}, project) {
		t.Fatal("lookout from a Project subdirectory was not matched")
	}
	if processBelongsToProject(lookoutProcess{WorkingDir: "/workspace/shopping"}, project) {
		t.Fatal("lookout from a sibling path was matched")
	}
}

func TestLiveLeadLookoutExcludesPollOnlyAndOtherProjects(t *testing.T) {
	project := store.Project{Root: "/workspace/shop", HerdrWorkspaceID: "w1", LeadPaneID: "w1:p1"}
	pid := os.Getpid()
	if liveLeadLookout(project, []lookoutProcess{{PID: pid, PaneID: "w1:p2", WorkingDir: project.Root, PollOnly: true}}) {
		t.Fatal("poll-only Project process is not the Lead delivery watcher")
	}
	if liveLeadLookout(project, []lookoutProcess{{PID: pid, PaneID: "w1:p4", WorkingDir: "/workspace/other"}}) {
		t.Fatal("other Project's watcher in the same workspace is not this Lead's watcher")
	}
	if !liveLeadLookout(project, []lookoutProcess{{PID: pid, PaneID: "w1:p1", WorkingDir: project.Root}}) {
		t.Fatal("live Lead watcher was not detected")
	}
	if liveLeadLookout(project, []lookoutProcess{{PID: -1, PaneID: "w1:p1", WorkingDir: project.Root}}) {
		t.Fatal("dead watcher blocked the typed fallback")
	}
}

func TestStopLookoutsRefusesUnverifiedExecutableIdentity(t *testing.T) {
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
	_, err = stopLookoutProcesses(context.Background(), t.TempDir(), []lookoutProcess{{PID: command.Process.Pid, Project: "shop", Kind: "lead"}})
	if err == nil {
		t.Fatal("stopLookoutProcesses accepted a process without verified executable identity")
	}
	if !lookoutPIDRunning(command.Process.Pid) {
		t.Fatalf("refusing the unverified process still terminated PID %d", command.Process.Pid)
	}
}

func TestStopLookoutsIgnoresProcessThatExitedBeforeStop(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if !lookoutPIDRunning(pid) {
		t.Fatal("test process did not start")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait() // The killed process exits with a signal status.

	_, err := stopLookoutProcesses(context.Background(), t.TempDir(), []lookoutProcess{{
		PID:        pid,
		Executable: lookoutExecutableIdentity{Path: "/proc/exited/exe", Inode: 1},
		Project:    "shop",
		Kind:       "lead",
	}})
	if err != nil {
		t.Fatalf("stopping an already exited Lookout: %v", err)
	}
}

func TestStopLookoutsEscalatesToSIGKILL(t *testing.T) {
	previousVerifier := lookoutExecutableVerifier
	lookoutExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { lookoutExecutableVerifier = previousVerifier })
	previousGracePeriod := lookoutStopGracePeriod
	lookoutStopGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { lookoutStopGracePeriod = previousGracePeriod })
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
	executable, ok := inspectLookoutExecutable(command.Process.Pid)
	if !ok {
		t.Fatal("could not inspect the test process executable")
	}
	processes, err := stopLookoutProcesses(context.Background(), t.TempDir(), []lookoutProcess{{PID: command.Process.Pid, Executable: executable, Project: "shop", Kind: "lead"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 1 || !processes[0].ForceKilled || lookoutPIDRunning(command.Process.Pid) {
		t.Fatalf("SIGTERM-ignoring process was not killed: %#v", processes)
	}
}

func TestLookoutPollerEnvironmentIsDetachedFromHerdrIdentity(t *testing.T) {
	project := store.Project{ID: 42, Name: "shop", Root: "/workspace/shop"}
	env := lookoutPollerEnvironment([]string{
		"PATH=/usr/bin", "POSSE_HOME=/tmp/old", "HERDR_PANE_ID=w1:p4", "HERDR_WORKSPACE_ID=w1", "HERDR_TAB_ID=w1:t1", "HERDR_ENV=1", "HERDR_PLUGIN_EVENT=focus", "CUSTOM=value",
	}, "/tmp/posse", project, "random-token", 1234)
	got := parseProcessEnvironment(strings.Join(env, "\x00"))
	if got["POSSE_HOME"] != "/tmp/posse" || got[lookoutPollerProjectEnv] != "42" || got[lookoutPollerTokenEnv] != "random-token" || got[lookoutPollerStartedEnv] != "1234" || got["PWD"] != project.Root {
		t.Fatalf("poller identity environment = %#v", got)
	}
	for _, key := range []string{"HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID", "HERDR_ENV", "HERDR_PLUGIN_EVENT"} {
		if _, found := got[key]; found {
			t.Errorf("detached poller inherited %s", key)
		}
	}
	if got["CUSTOM"] != "value" {
		t.Fatalf("poller lost unrelated environment: %#v", got)
	}
}

func TestLookoutProcessListingExtractsPollerIdentity(t *testing.T) {
	listing := "31 /usr/local/bin/posse lookout --poll-only POSSE_HOME=/tmp/shop POSSE_LOOKOUT_PROJECT_ID=8 POSSE_LOOKOUT_TOKEN=abc POSSE_LOOKOUT_STARTED_AT=900 PWD=/workspace/shop\n"
	processes := lookoutProcessesInListingWithVerifier(listing, "/tmp/shop", func(path string) bool { return filepath.Base(path) == "posse" })
	if len(processes) != 1 {
		t.Fatalf("poller processes = %#v", processes)
	}
	process := processes[0]
	if process.Kind != "poller" || !process.PollOnly || process.PollerProjectID != 8 || process.PollerToken != "abc" || process.PollerStartedAt != 900 || process.WorkingDir != "/workspace/shop" {
		t.Fatalf("poller identity = %#v", process)
	}
}

func TestLookoutPollerHealthReportsMissingProcess(t *testing.T) {
	ctx := context.Background()
	db, project, home := newLookoutTestProject(t)
	defer db.Close()
	project.HerdrWorkspaceID = "w1"
	service := testService(home, nil)
	check, err := service.lookoutPollerHealth(ctx, db, home, project)
	if err != nil {
		t.Fatal(err)
	}
	if check.Status != "warn" || !strings.Contains(check.Detail, "poller=missing") {
		t.Fatalf("missing poller health = %#v", check)
	}
}
