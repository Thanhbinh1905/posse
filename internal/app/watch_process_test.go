package app

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
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
	if processes[1].PID != 23 || processes[1].Kind != "watch_tab" || !processes[1].PollOnly {
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

func TestLiveLeadWatchExcludesPollOnlyAndOtherProjects(t *testing.T) {
	project := store.Project{Root: "/workspace/shop", HerdrWorkspaceID: "w1", LeadPaneID: "w1:p1"}
	pid := os.Getpid()
	if liveLeadWatch(project, []watchProcess{{PID: pid, PaneID: "w1:p2", WorkingDir: project.Root, PollOnly: true}}) {
		t.Fatal("poll-only Watch tab is not the Lead watcher")
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
		t.Fatal("stopLookoutProcesses accepted a process without verified executable identity")
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

func TestFreshWatchPaneWaitsWithoutBlockingPastStartupGrace(t *testing.T) {
	previousGrace := watchStartupGracePeriod
	watchStartupGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { watchStartupGracePeriod = previousGrace })
	ctx := context.Background()
	db, project, home := newWatchTestProject(t)
	defer db.Close()
	fake := herdr.NewFake()
	project.HerdrWorkspaceID = "w1"
	pane := herdr.Pane{PaneID: "fresh:p1", TabID: "fresh:t1", WorkspaceID: "w1", Label: watchTabLabel(project)}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{pane}}
	service := testService(home, fake)
	t.Setenv("HERDR_PANE_ID", "")
	startedAt := time.Now()
	if err := service.ensureWatchTab(ctx, db, project, fake.SnapshotValue, false); err != nil {
		t.Fatal(err)
	}
	finishedAt := time.Now()
	if elapsed := finishedAt.Sub(startedAt); elapsed >= 500*time.Millisecond {
		t.Fatalf("startup grace blocked Watch reconciliation for %s", elapsed)
	}
	if fake.CallCount("tab.close") != 0 || fake.CallCount("tab.create") != 0 {
		t.Fatalf("fresh pane was replaced inside its startup grace: %#v", fake.Calls)
	}
	// The short test grace can expire before this database read under -race.
	time.Sleep(watchStartupGracePeriod + 10*time.Millisecond)
	state, err := db.WatchRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	graceStarted, graceFinished := startedAt.Add(watchStartupGracePeriod).UnixMilli(), finishedAt.Add(watchStartupGracePeriod).UnixMilli()
	if state.PaneID != pane.PaneID || state.RetryAt < graceStarted || state.RetryAt > graceFinished || state.FailedStarts != 0 || state.NoticeRaised {
		t.Fatalf("fresh pane grace state = %#v; retry deadline should be between %d and %d", state, graceStarted, graceFinished)
	}
	if err := service.ensureWatchTab(ctx, db, project, fake.SnapshotValue, false); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("tab.close") != 1 || fake.CallCount("tab.create") != 1 {
		t.Fatalf("expired startup grace did not replace the pane: %#v", fake.Calls)
	}
}

func TestWatchRetryBackoffIsExponentialAndBounded(t *testing.T) {
	if got := watchRetryBackoff(1); got != watchRetryBasePeriod {
		t.Fatalf("first retry backoff = %s, want %s", got, watchRetryBasePeriod)
	}
	if got := watchRetryBackoff(2); got != 2*watchRetryBasePeriod {
		t.Fatalf("second retry backoff = %s, want %s", got, 2*watchRetryBasePeriod)
	}
	if got := watchRetryBackoff(100); got != watchRetryMaxPeriod {
		t.Fatalf("retry backoff = %s, want cap %s", got, watchRetryMaxPeriod)
	}
}

func TestRepeatedWatchStartupFailuresRaiseOneNotice(t *testing.T) {
	ctx := context.Background()
	db, project, _ := newWatchTestProject(t)
	defer db.Close()
	for attempt := 1; attempt <= watchStartFailureNoticeAfter; attempt++ {
		if err := markWatchCreated(ctx, db, project.ID, fmt.Sprintf("pane-%d", attempt), true); err != nil {
			t.Fatal(err)
		}
	}
	failures, noticeRaised, err := watchStartFailureNotice(ctx, db, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failures != watchStartFailureNoticeAfter || noticeRaised {
		t.Fatalf("repeated failure state = (%d, %v), want (%d, false)", failures, noticeRaised, watchStartFailureNoticeAfter)
	}
	if err := markWatchStartFailureNotice(ctx, db, project.ID); err != nil {
		t.Fatal(err)
	}
	failures, noticeRaised, err = watchStartFailureNotice(ctx, db, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failures != watchStartFailureNoticeAfter || !noticeRaised {
		t.Fatalf("notice state = (%d, %v), want (%d, true)", failures, noticeRaised, watchStartFailureNoticeAfter)
	}
}

func TestUnknownWatchProcessIsReplacedWithoutTypingIntoOldPane(t *testing.T) {
	ctx := context.Background()
	db, project, home := newWatchTestProject(t)
	defer db.Close()
	project.HerdrWorkspaceID = "w1"
	fake := herdr.NewFake()
	pane := herdr.Pane{PaneID: "stale:p9", TabID: "stale:t9", WorkspaceID: "w1", Label: watchTabLabel(project)}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{pane}}
	service := testService(home, fake)
	if err := db.SetWatchRecovery(ctx, project.ID, store.WatchRecovery{PaneID: pane.PaneID, RetryAt: time.Now().Add(-time.Second).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", "")
	if err := service.ensureWatchTab(ctx, db, project, fake.SnapshotValue, false); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("tab.close") != 1 || fake.CallCount("tab.create") != 1 {
		t.Fatalf("stale tab was not replaced: %#v", fake.Calls)
	}
	for _, call := range fake.Calls {
		if call.Method == "pane.send_input" && call.Params["pane_id"] == pane.PaneID {
			t.Fatal("queued a new command into an unknown live Watch pane")
		}
	}
}
