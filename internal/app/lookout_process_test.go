package app

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutProcessListingMatchesPaneAndPosseHome(t *testing.T) {
	listing := "12 /usr/local/bin/posse lookout --poll-only HERDR_PANE_ID=w1:p9 POSSE_HOME=/tmp/other HOME=/tmp\n" +
		"13 /usr/local/bin/posse lookout --poll-only HERDR_PANE_ID=w1:p2 POSSE_HOME=/tmp/shop HOME=/tmp\n"
	if !lookoutProcessInListing(listing, "w1:p2", "/tmp/shop") {
		t.Fatal("Darwin process listing missed matching Lookout")
	}
	if lookoutProcessInListing(listing, "w1:p9", "/tmp/shop") {
		t.Fatal("mixed separate Lookout homes")
	}
}

func TestLookoutProcessListingRecognizesLeadWatchersRenamedBinariesAndDefaultHome(t *testing.T) {
	listing := "21 /nix/store/posse/bin/posse lookout --json --quiet-routine HERDR_PANE_ID=w1:p1 POSSE_HOME=/tmp/shop HOME=/tmp PWD=/tmp/shop\n" +
		"22 /usr/local/bin/posse lookout --ack 9 HOME=/home/user POSSE_HOME=/home/user/.posse PWD=/repo\n" +
		"23 /usr/local/bin/renamed-posse lookout --poll-only POSSE_HOME=/tmp/shop HOME=/tmp\n"
	processes := lookoutProcessesInListing(listing, "/tmp/shop")
	if len(processes) != 2 {
		t.Fatalf("found %d lookouts, want the matching-home Lead and renamed poll-only binary: %#v", len(processes), processes)
	}
	if processes[0].PID != 21 || processes[0].Kind != "lead" || !processes[0].QuietRoutine || processes[0].PaneID != "w1:p1" {
		t.Fatalf("Lead watcher = %#v", processes[0])
	}
	if processes[1].PID != 23 || processes[1].Kind != "lookout_tab" || !processes[1].PollOnly {
		t.Fatalf("renamed poll-only watcher = %#v", processes[1])
	}
	if _, found := lookoutFromArgs(24, []string{"/bin/sh", "/tmp/renamed-posse", "lookout", "--poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop"}, "/tmp/shop"); !found {
		t.Fatal("missed a renamed Lookout executable invoked through a shell script")
	}
	if _, found := lookoutFromArgs(25, []string{"/bin/sh", "-c", "posse lookout --poll-only"}, map[string]string{"POSSE_HOME": "/tmp/shop"}, "/tmp/shop"); found {
		t.Fatal("matched a shell whose command text contains a Lookout invocation")
	}
	if got := processHome(map[string]string{"HOME": "/home/user"}); got != "/home/user/.posse" {
		t.Fatalf("default process home = %q", got)
	}
	defaultHome := lookoutProcessesInListing("24 /usr/local/bin/posse lookout HOME=/home/user PWD=/repo\n", "/home/user/.posse")
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

func TestStopLookoutsEscalatesToSIGKILL(t *testing.T) {
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
	processes, err := stopLookoutProcesses(context.Background(), t.TempDir(), []lookoutProcess{{PID: command.Process.Pid, Project: "shop", Kind: "lead"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) != 1 || !processes[0].ForceKilled || lookoutPIDRunning(command.Process.Pid) {
		t.Fatalf("SIGTERM-ignoring process was not killed: %#v", processes)
	}
}

func TestFreshLookoutPaneIsNotReplacedBeforeStartupGraceExpires(t *testing.T) {
	previousGrace := lookoutStartupGracePeriod
	lookoutStartupGracePeriod = 50 * time.Millisecond
	t.Cleanup(func() { lookoutStartupGracePeriod = previousGrace })
	home := t.TempDir()
	fake := herdr.NewFake()
	project := store.Project{Name: "shop", Root: home, HerdrWorkspaceID: "w1"}
	pane := herdr.Pane{PaneID: "fresh:p1", TabID: "fresh:t1", WorkspaceID: "w1", Label: lookoutTabLabel(project)}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{pane}}
	service := testService(home, fake)
	t.Setenv("HERDR_PANE_ID", "")
	startedAt := time.Now()
	if err := service.ensureLookoutTab(context.Background(), project, fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed < lookoutStartupGracePeriod {
		t.Fatalf("fresh startup pane replaced after %s, before grace period %s", elapsed, lookoutStartupGracePeriod)
	}
	if fake.CallCount("tab.close") != 1 || fake.CallCount("tab.create") != 1 {
		t.Fatalf("stale pane was not replaced after grace expired: %#v", fake.Calls)
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
	service := &Service{}
	const projectID = int64(7)
	for attempt := 1; attempt <= lookoutStartFailureNoticeAfter; attempt++ {
		service.markLookoutCreated(projectID, fmt.Sprintf("pane-%d", attempt), true)
	}
	failures, noticeRaised := service.lookoutStartFailureNotice(projectID)
	if failures != lookoutStartFailureNoticeAfter || noticeRaised {
		t.Fatalf("repeated failure state = (%d, %v), want (%d, false)", failures, noticeRaised, lookoutStartFailureNoticeAfter)
	}
	service.markLookoutStartFailureNotice(projectID)
	failures, noticeRaised = service.lookoutStartFailureNotice(projectID)
	if failures != lookoutStartFailureNoticeAfter || !noticeRaised {
		t.Fatalf("notice state = (%d, %v), want (%d, true)", failures, noticeRaised, lookoutStartFailureNoticeAfter)
	}
}

func TestUnknownLookoutProcessIsReplacedWithoutTypingIntoOldPane(t *testing.T) {
	home := t.TempDir()
	fake := herdr.NewFake()
	project := store.Project{Name: "shop", Root: home, HerdrWorkspaceID: "w1"}
	pane := herdr.Pane{PaneID: "stale:p9", TabID: "stale:t9", WorkspaceID: "w1", Label: lookoutTabLabel(project)}
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{pane}}
	service := testService(home, fake)
	service.lookoutRecovery = map[int64]lookoutRecoveryState{project.ID: {paneID: pane.PaneID, retryAt: time.Now().Add(-time.Second)}}
	t.Setenv("HERDR_PANE_ID", "")
	if err := service.ensureLookoutTab(context.Background(), project, fake.SnapshotValue); err != nil {
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
