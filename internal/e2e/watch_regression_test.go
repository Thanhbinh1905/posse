//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func watchPanes(t *testing.T, client *herdr.Client) []herdr.Pane {
	t.Helper()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var panes []herdr.Pane
	for _, pane := range snapshot.Panes {
		if pane.Label == "posse:shop:watch" {
			panes = append(panes, pane)
		}
	}
	return panes
}

// lookoutPIDs lists poll-only Watch processes that belong to this fixture.
func watchPIDs(root string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if !strings.Contains(string(cmdline), "--poll-only") {
			continue
		}
		environ, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if strings.Contains(string(environ), root) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// A dead Watch leaves a labeled tab while Herdr and the Lead stay up.
// The Lead's ordinary watch tick must restart it without `posse up`.
func TestWatchRestartsAfterProcessExit(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Watch process not running: %v", watchPIDs(f.root))
	}
	for _, pid := range watchPIDs(f.root) {
		_ = exec.Command("kill", "-INT", strconv.Itoa(pid)).Run()
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatal("Watch did not exit")
	}
	panes := watchPanes(t, client)
	t.Logf("Watch panes after exit: %d", len(panes))
	command := exec.Command(f.binary, "watch", "--ack", "all", "--timeout", "10000")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Lead watch: %v %s", err, output)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Errorf("no Watch process after Lead tick; panes labelled Watch=%d", len(watchPanes(t, client)))
	}
}

func TestWatchDoesNotRecreateDuringSlowLoginShellStartup(t *testing.T) {
	f := newPRLifecycleFixtureWithSlowLoginShell(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Watch process not running: %v", watchPIDs(f.root))
	}

	marker := filepath.Join(f.root, "slow-login-enabled")
	starts := filepath.Join(f.root, "slow-login-starts")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	panes := watchPanes(t, client)
	if len(panes) != 1 {
		t.Fatalf("Watch tabs before close = %d, want 1", len(panes))
	}
	for _, pid := range watchPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatal("initial poll-only process did not stop")
	}
	if err := f.db.ResetWatchRecovery(context.Background(), f.project.ID); err != nil {
		t.Fatal(err)
	}
	createdTabs := make(map[string]struct{})
	stopSampling := make(chan struct{})
	samplingDone := make(chan struct{})
	samplingStopped := false
	stopSampler := func() {
		if samplingStopped {
			return
		}
		close(stopSampling)
		<-samplingDone
		samplingStopped = true
	}
	go func() {
		defer close(samplingDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-ticker.C:
				snapshot, err := client.Snapshot(context.Background())
				if err != nil {
					continue
				}
				for _, tab := range snapshot.Tabs {
					if tab.Label == "posse:shop:watch" && tab.TabID != panes[0].TabID {
						createdTabs[tab.TabID] = struct{}{}
					}
				}
			}
		}
	}()
	defer stopSampler()

	command := exec.Command(f.binary, "watch", "--ack", "all", "--timeout", "3500")
	command.Dir, command.Env = f.repo, f.leadEnv
	output := make(chan []byte, 1)
	commandDone := make(chan error, 1)
	go func() {
		result, err := command.CombinedOutput()
		output <- result
		commandDone <- err
	}()
	if !waitForCondition(5*time.Second, func() bool {
		state, err := f.db.WatchRecovery(context.Background(), f.project.ID)
		return err == nil && state.PaneID == panes[0].PaneID && state.RetryAt > time.Now().UnixMilli()
	}) {
		t.Fatal("Lead watch did not record its observed startup grace before tab close")
	}
	if _, err := client.Call(context.Background(), "tab.close", map[string]any{"tab_id": panes[0].TabID}); err != nil {
		t.Fatalf("close Watch tab: %v", err)
	}
	select {
	case err := <-commandDone:
		if err != nil {
			t.Fatalf("Lead watch: %v %s", err, <-output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Lead watch did not finish its bounded wait")
	}

	stopSampler()
	if len(createdTabs) != 1 {
		t.Fatalf("Watch recovery created %d tabs, want exactly 1; login starts=%d; tab IDs=%v", len(createdTabs), lineCount(starts), createdTabs)
	}
	if lineCount(starts) == 0 {
		t.Fatal("slow login shell profile did not run")
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Watch process did not stay up: %v", watchPIDs(f.root))
	}
	if panes := watchPanes(t, client); len(panes) != 1 {
		t.Fatalf("Watch tabs after recovery = %d, want 1", len(panes))
	}
}

func lineCount(path string) int {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(contents), "started\n")
}

// L2: Herdr restarts. Recovery restarts the Lead but must also restore the
// Watch tab, or nothing polls while the Lead is idle.
func TestWatchRestartsAfterHerdrRestart(t *testing.T) {
	checkWatchRestore(t, false)
}

func TestWatchRestartsWithoutRecordedGeneration(t *testing.T) {
	checkWatchRestore(t, true)
}

func checkWatchRestore(t *testing.T, clearGeneration bool) {
	t.Helper()
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if len(watchPanes(t, client)) != 1 {
		t.Fatal("Watch tab missing before restart")
	}
	if clearGeneration {
		// The Watch's first tick might not have recorded the server generation
		// before Herdr exits. Pin that race rather than rely on its timing.
		if _, err := f.db.ExecContext(context.Background(), `DELETE FROM project_runtime WHERE project_id=?`, f.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	beforeGeneration, _ := f.db.ProjectServerStartedAt(context.Background(), f.project.ID)
	if err := client.StopIsolatedServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	startServer(t, client)
	env := f.env
	for _, key := range []string{"HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID"} {
		env = setEnv(env, key, "")
	}
	command := exec.Command(f.binary, "recover", "--all")
	command.Dir, command.Env = f.repo, env
	output, err := command.CombinedOutput()
	t.Logf("recover --all: %v %s", err, output)
	project, err := f.db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := client.Snapshot(context.Background())
	leadFound := false
	for _, pane := range snapshot.Panes {
		if pane.PaneID == project.LeadPaneID {
			leadFound = true
		}
	}
	t.Logf("Lead pane restored=%v workspace=%s", leadFound, project.HerdrWorkspaceID)
	if !waitForCondition(15*time.Second, func() bool {
		return len(watchPanes(t, client)) > 0 && len(watchPIDs(f.root)) > 0
	}) {
		for _, pane := range watchPanes(t, client) {
			visible, readErr := client.Call(context.Background(), "pane.read", map[string]any{"pane_id": pane.PaneID, "source": "visible"})
			t.Logf("watch pane %s status=%s visible=%q readError=%v", pane.PaneID, pane.AgentStatus, visible, readErr)
		}
		afterGeneration, _ := f.db.ProjectServerStartedAt(context.Background(), f.project.ID)
		t.Errorf("Watch not restored after Herdr restart: panes=%d processes=%d generation=%q -> %q", len(watchPanes(t, client)), len(watchPIDs(f.root)), beforeGeneration, afterGeneration)
	}
	_ = store.StateMerged
}

// L3: the Watch tab and ordinary Lead commands both run automatic Teardown.
// A merged PR must be torn down without a false "teardown incomplete" reason.
func TestConcurrentWatchTeardownRaisesNoFalseReason(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "watch-follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1")
	candidate := f.mustTask(t, "t1")
	merge := f.mergeOnLocalOrigin(t, candidate, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, candidate.GatedSHA)
	showAfterActiveTeardown(t, f, "t1")
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notices {
		if n.Kind == "teardown_incomplete" {
			t.Errorf("state=%s false reason: %s", f.mustTask(t, "t1").State, n.Summary)
		}
	}
}
