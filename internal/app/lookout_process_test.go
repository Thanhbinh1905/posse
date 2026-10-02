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
	if processes[1].PID != 23 || processes[1].Kind != "lookout_tab" || !processes[1].PollOnly {
		t.Fatalf("renamed poll-only watcher = %#v", processes[1])
	}
	if _, found := lookoutFromArgs(25, []string{"/bin/sh", "-c", "posse lookout --poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop"}, "/tmp/shop"); found {
		t.Fatal("matched a shell whose command text contains a Lookout invocation")
	}
	if _, found := lookoutFromArgs(26, []string{"/tmp/renamed-posse", "lookout", "--poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop"}, "/tmp/shop"); !found {
		t.Fatal("missed the renamed binary's CLI argument pattern")
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
		t.Fatal("poll-only Lookout tab is not the Lead watcher")
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

func TestFreshLookoutPaneWaitsWithoutBlockingPastStartupGrace(t *testing.T) {
	previousGrace := lookoutStartupGracePeriod
	lookoutStartupGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { lookoutStartupGracePeriod = previousGrace })
	ctx := context.Background()
	db, project, home := newLookoutTestProject(t)
	defer db.Close()
	fake := herdr.NewFake()
	project.HerdrWorkspaceID = "w1"
	pane := herdr.Pane{PaneID: "fresh:p1", TabID: "fresh:t1", WorkspaceID: "w1", Label: lookoutTabLabel(project)}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{pane}}
	service := testService(home, fake)
	t.Setenv("HERDR_PANE_ID", "")
	startedAt := time.Now()
	if err := service.ensureLookoutTab(ctx, db, project, fake.SnapshotValue, false); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 500*time.Millisecond {
		t.Fatalf("startup grace blocked Lookout reconciliation for %s", elapsed)
	}
	if fake.CallCount("tab.close") != 0 || fake.CallCount("tab.create") != 0 {
		t.Fatalf("fresh pane was replaced inside its startup grace: %#v", fake.Calls)
	}
	state, err := db.LookoutRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.PaneID != pane.PaneID || state.RetryAt <= time.Now().UnixMilli() {
		t.Fatalf("fresh pane grace state = %#v", state)
	}
	state.RetryAt = time.Now().Add(-time.Second).UnixMilli()
	if err := db.SetLookoutRecovery(ctx, project.ID, state); err != nil {
		t.Fatal(err)
	}
	if err := service.ensureLookoutTab(ctx, db, project, fake.SnapshotValue, false); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("tab.close") != 1 || fake.CallCount("tab.create") != 1 {
		t.Fatalf("expired startup grace did not replace the pane: %#v", fake.Calls)
	}
}

func TestLookoutRetryBackoffIsExponentialAndBounded(t *testing.T) {
	if got := lookoutRetryBackoff(1); got != lookoutRetryBasePeriod {
		t.Fatalf("first retry backoff = %s, want %s", got, lookoutRetryBasePeriod)
	}
	if got := lookoutRetryBackoff(2); got != 2*lookoutRetryBasePeriod {
		t.Fatalf("second retry backoff = %s, want %s", got, 2*lookoutRetryBasePeriod)
	}
	if got := lookoutRetryBackoff(100); got != lookoutRetryMaxPeriod {
		t.Fatalf("retry backoff = %s, want cap %s", got, lookoutRetryMaxPeriod)
	}
}

func TestRepeatedLookoutStartupFailuresRaiseOneNotice(t *testing.T) {
	ctx := context.Background()
	db, project, _ := newLookoutTestProject(t)
	defer db.Close()
	for attempt := 1; attempt <= lookoutStartFailureNoticeAfter; attempt++ {
		if err := markLookoutCreated(ctx, db, project.ID, fmt.Sprintf("pane-%d", attempt), true); err != nil {
			t.Fatal(err)
		}
	}
	failures, noticeRaised, err := lookoutStartFailureNotice(ctx, db, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failures != lookoutStartFailureNoticeAfter || noticeRaised {
		t.Fatalf("repeated failure state = (%d, %v), want (%d, false)", failures, noticeRaised, lookoutStartFailureNoticeAfter)
	}
	if err := markLookoutStartFailureNotice(ctx, db, project.ID); err != nil {
		t.Fatal(err)
	}
	failures, noticeRaised, err = lookoutStartFailureNotice(ctx, db, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failures != lookoutStartFailureNoticeAfter || !noticeRaised {
		t.Fatalf("notice state = (%d, %v), want (%d, true)", failures, noticeRaised, lookoutStartFailureNoticeAfter)
	}
}

func TestUnknownLookoutProcessIsReplacedWithoutTypingIntoOldPane(t *testing.T) {
	ctx := context.Background()
	db, project, home := newLookoutTestProject(t)
	defer db.Close()
	project.HerdrWorkspaceID = "w1"
	fake := herdr.NewFake()
	pane := herdr.Pane{PaneID: "stale:p9", TabID: "stale:t9", WorkspaceID: "w1", Label: lookoutTabLabel(project)}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{pane}}
	service := testService(home, fake)
	if err := db.SetLookoutRecovery(ctx, project.ID, store.LookoutRecovery{PaneID: pane.PaneID, RetryAt: time.Now().Add(-time.Second).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", "")
	if err := service.ensureLookoutTab(ctx, db, project, fake.SnapshotValue, false); err != nil {
		t.Fatal(err)
	}
	if fake.CallCount("tab.close") != 1 || fake.CallCount("tab.create") != 1 {
		t.Fatalf("stale tab was not replaced: %#v", fake.Calls)
	}
	for _, call := range fake.Calls {
		if call.Method == "pane.send_input" && call.Params["pane_id"] == pane.PaneID {
			t.Fatal("queued a new command into an unknown live Lookout pane")
		}
	}
}
