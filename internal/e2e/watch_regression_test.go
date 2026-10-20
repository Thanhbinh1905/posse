//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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

// watchPIDs lists hidden poll-only Watch processes that belong to this fixture.
func watchPIDs(root string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		processRoot := filepath.Join("/proc", entry.Name())
		stat, err := os.ReadFile(filepath.Join(processRoot, "stat"))
		if err != nil {
			continue // The process exited between ReadDir and inspection.
		}
		if closeParen := strings.LastIndexByte(string(stat), ')'); closeParen >= 0 {
			fields := strings.Fields(string(stat[closeParen+1:]))
			if len(fields) > 0 && fields[0] == "Z" {
				continue // An exited zombie is not a running Watch.
			}
		}
		cmdline, _ := os.ReadFile(filepath.Join(processRoot, "cmdline"))
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

func stopFixtureWatchRuntimes(root string) {
	pids := watchPIDs(root)
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if waitForCondition(3*time.Second, func() bool { return len(watchPIDs(root)) == 0 }) {
		return
	}
	for _, pid := range watchPIDs(root) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = waitForCondition(2*time.Second, func() bool { return len(watchPIDs(root)) == 0 })
}

func TestWatchRestartsAfterProcessExit(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Watch process not running: %v", watchPIDs(f.root))
	}
	before := watchPIDs(f.root)[0]
	if err := syscall.Kill(before, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("killed Watch process remains visible: %v", watchPIDs(f.root))
	}
	output := runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if !strings.Contains(output, `"check":"Project Watch shop"`) {
		t.Fatalf("doctor omitted watch health: %s", output)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("the next Project command did not repair the watch: %v\n%s", watchPIDs(f.root), output)
	}
	if panes := watchPanes(t, client); len(panes) != 0 {
		t.Fatalf("hidden watch opened %d visible Watch panes: %#v", len(panes), panes)
	}
}

func TestWatchDoesNotOpenLoginShellOrVisiblePane(t *testing.T) {
	f := newPRLifecycleFixtureWithSlowLoginShell(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Project watch did not start: %v", watchPIDs(f.root))
	}
	if panes := watchPanes(t, client); len(panes) != 0 {
		t.Fatalf("Watch opened visible panes: %#v", panes)
	}
	if starts := lineCount(filepath.Join(f.root, "slow-login-starts")); starts != 0 {
		t.Fatalf("hidden watch started %d login shells", starts)
	}
}

func lineCount(path string) int {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(contents), "started\n")
}

func TestHiddenWatchRuntimeDeliversNoticesWhenLeadHasNoWait(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Project watch did not start: %v", watchPIDs(f.root))
	}
	if _, err := client.Run(context.Background(), "pane", "report-agent", f.project.LeadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatalf("mark idle Lead: %v", err)
	}
	other, err := createTab(client, f.project.HerdrWorkspaceID, f.repo, "fixture:other")
	if err != nil {
		t.Fatalf("create unfocused pane: %v", err)
	}
	if _, err := client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": other.RootPane.PaneID}); err != nil {
		t.Fatalf("unfocus Lead: %v", err)
	}
	noticeID, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "Worker finished"})
	if err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		return noticeDeliveredAt(t, f.db, f.project.ID, noticeID) != 0
	}) {
		notices, _ := f.db.Notices(context.Background(), f.project.ID, false)
		t.Fatalf("hidden watch did not deliver a Notice without a Lead-side wait: %#v", notices)
	}
	contents, err := os.ReadFile(filepath.Join(f.root, "lead-prompts.log"))
	if err != nil || !strings.Contains(string(contents), "Worker finished") {
		t.Fatalf("hidden watch did not wake the idle Lead: %s (%v)", contents, err)
	}
}

func noticeDeliveredAt(t *testing.T, db *store.DB, projectID, noticeID int64) int64 {
	t.Helper()
	notices, err := db.Notices(context.Background(), projectID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.ID == noticeID {
			return notice.DeliveredAt
		}
	}
	return 0
}

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
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatal("hidden watch missing before Herdr restart")
	}
	if len(watchPanes(t, client)) != 0 {
		t.Fatal("Project has a visible Watch pane before Herdr restart")
	}
	if clearGeneration {
		if _, err := f.db.ExecContext(context.Background(), `DELETE FROM project_runtime WHERE project_id=?`, f.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.StopIsolatedServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		_, err := client.Snapshot(context.Background())
		return err != nil
	}) {
		t.Fatal("isolated Herdr server remained available after stop")
	}
	startServer(t, client)
	env := f.env
	for _, key := range []string{"HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID"} {
		env = setEnv(env, key, "")
	}
	command := exec.Command(f.binary, "recover", "--all")
	command.Dir, command.Env = f.repo, env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("recover --all after Herdr restart: %v\n%s", err, output)
	}
	if !waitForCondition(15*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden watch missing after Herdr restart: %v\n%s", watchPIDs(f.root), output)
	}
	if panes := watchPanes(t, client); len(panes) != 0 {
		t.Fatalf("Herdr restart restored %d visible Watch panes: %#v", len(panes), panes)
	}
}

func TestConcurrentWatchTeardownRaisesNoFalseReason(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "watch-follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.startAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t1")
	candidate := f.mustTask(t, "t1")
	merge := f.mergeOnLocalOrigin(t, candidate, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, candidate.GatedSHA)
	showAfterActiveTeardown(t, f, "t1")
	assertNoValidationFixRound(t, f, "t1", "gate")
	assertNoValidationFixRound(t, f, "t1", "freshness")
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "teardown_incomplete" {
			t.Errorf("state=%s false reason: %s", f.mustTask(t, "t1").State, notice.Summary)
		}
	}
}

func TestUpClosesLegacyVisibleWatchTab(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if _, err := client.Call(context.Background(), "tab.create", map[string]any{
		"workspace_id": f.project.HerdrWorkspaceID, "cwd": f.repo, "label": "posse:shop:watch", "focus": false,
	}); err != nil {
		t.Fatalf("create legacy Watch tab: %v", err)
	}
	command := exec.Command(f.binary, "up", "--name", "shop")
	command.Dir, command.Env = f.repo, f.leadEnv
	_, _ = command.CombinedOutput() // A live Lead may make `up` return lead_running after migration.
	if panes := watchPanes(t, client); len(panes) != 0 {
		t.Fatalf("legacy Watch tab survived posse up: %#v", panes)
	}
}

func TestWatchRuntimeReexecsAfterBinaryReplacement(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch missing before update simulation: %v", watchPIDs(f.root))
	}
	pid := watchPIDs(f.root)[0]
	before, err := f.db.WatchRuntime(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	replacement := f.binary + ".replacement"
	if err := os.WriteFile(replacement, binary, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, f.binary); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	executable, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("replacement binary has no file identity")
	}
	if !waitForCondition(15*time.Second, func() bool {
		state, stateErr := f.db.WatchRuntime(context.Background(), f.project.ID)
		return stateErr == nil && state.PID == pid && state.ExecutableInode == executable.Ino && len(watchPIDs(f.root)) == 1
	}) {
		state, _ := f.db.WatchRuntime(context.Background(), f.project.ID)
		t.Fatalf("watch did not re-exec into the replacement binary: before=%#v after=%#v pids=%v", before, state, watchPIDs(f.root))
	}
}

func TestWatchHandoffRepairsDeadPoller(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("initial Project watch missing: %v", watchPIDs(f.root))
	}
	oldPID := watchPIDs(f.root)[0]
	if err := syscall.Kill(oldPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("killed watch remains: %v", watchPIDs(f.root))
	}

	runPosse(t, f.binary, f.repo, f.leadEnv, "watch", "--json", "--handoff", "--destination", "pi:repair", "--timeout", "1")
	if !waitForCondition(10*time.Second, func() bool {
		pids := watchPIDs(f.root)
		return len(pids) == 1 && pids[0] != oldPID
	}) {
		t.Fatalf("a Pi extension handoff did not repair the hidden Watch: %v", watchPIDs(f.root))
	}
}

func TestWatchPollerUsesItsSpawnedBinaryInsteadOfSetupManifest(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("initial Project watch missing: %v", watchPIDs(f.root))
	}
	oldPID := watchPIDs(f.root)[0]
	if err := syscall.Kill(oldPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("killed watch remains: %v", watchPIDs(f.root))
	}

	binary, err := os.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	manifestBinary := filepath.Join(f.root, "manifest-posse")
	if err := os.WriteFile(manifestBinary, binary, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]string{"binary": manifestBinary})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "posse", "setup.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch did not restart: %v", watchPIDs(f.root))
	}
	pid := watchPIDs(f.root)[0]
	time.Sleep(7 * time.Second)
	path, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(path, " (deleted)") != f.binary {
		t.Fatalf("watch executable = %q, want spawned executable %q", path, f.binary)
	}
}

func TestWatchHeartbeatStopsWhenProjectMaintenanceStalls(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch did not start: %v", watchPIDs(f.root))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	releaseFetch := make(chan struct{})
	fetchStarted := make(chan struct{}, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		fetchStarted <- struct{}{}
		<-releaseFetch
		_ = connection.Close()
	}()
	t.Cleanup(func() {
		close(releaseFetch)
		_ = listener.Close()
		stopLiveWatchRuntime(t, f.root)
	})
	gitTest(t, f.env, f.repo, "config", "--unset-all", "url.file://"+f.remote+".insteadOf")
	gitTest(t, f.env, f.repo, "config", "url.http://"+listener.Addr().String()+"/.insteadOf", "https://github.com/acme/shop.git")
	if _, err := f.db.ExecContext(context.Background(), `UPDATE project_watch_state SET checkout_checked_at=0 WHERE project_id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fetchStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("Project Watch did not enter the deliberately stalled Git fetch")
	}
	before, err := f.db.WatchRuntime(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(7 * time.Second)
	during, err := f.db.WatchRuntime(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if during.HeartbeatAt != before.HeartbeatAt {
		t.Fatalf("Watch heartbeat advanced while Project maintenance was stalled: before=%d during=%d", before.HeartbeatAt, during.HeartbeatAt)
	}
}

func TestIngestDoesNotWaitForHealthyWatchRuntimeLock(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch did not start: %v", watchPIDs(f.root))
	}
	lock, err := os.OpenFile(filepath.Join(f.home, "projects", f.project.Name, "watch.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	event, err := json.Marshal(map[string]any{
		"event": "pane.agent_status_changed",
		"data":  map[string]string{"pane_id": f.project.LeadPaneID, "agent_status": "working"},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(f.binary, "_ingest")
	command.Dir, command.Env = f.repo, setEnv(f.env, "HERDR_PLUGIN_EVENT_JSON", string(event))
	started := time.Now()
	output, err := command.CombinedOutput()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("_ingest failed while Watch lock was held: %v\n%s", err, output)
	}
	t.Logf("_ingest completed in %s while the Watch lock was held", elapsed)
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("_ingest waited %s for a healthy Watch lock; want under 500 ms", elapsed)
	}
}

func TestDownStopsAWedgedProjectWatch(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch did not start: %v", watchPIDs(f.root))
	}
	pid := watchPIDs(f.root)[0]
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGCONT)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	command := exec.Command(f.binary, "down")
	command.Dir, command.Env = f.repo, f.env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("posse down could not stop wedged Watch: %v\n%s", err, output)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("posse down returned while Watch PID %d remained running", pid)
	}
}

func TestPassiveWatchLeavesCommandBoundaryIntentsForNormalRecovery(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch did not start: %v", watchPIDs(f.root))
	}
	ctx := context.Background()
	for sequence, command := range []string{"merge --execute", "teardown"} {
		taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
			Seq: sequence + 1, Type: "change", Title: command, MergeMode: "local",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.StartIntent(ctx, f.project.ID, taskID, command, "started", `{}`, 2147483000+sequence); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(3 * time.Second)
	intents, err := f.db.Intents(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 2 {
		t.Fatalf("passive Watch recovered command-boundary intents: %#v", intents)
	}
}

func TestWatchRepairsAfterHerdrAgentEvent(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("initial Project watch missing: %v", watchPIDs(f.root))
	}
	if err := syscall.Kill(watchPIDs(f.root)[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("killed watch remains: %v", watchPIDs(f.root))
	}
	event, err := json.Marshal(map[string]any{
		"event": "pane_agent_status_changed",
		"data":  map[string]string{"pane_id": f.project.LeadPaneID, "workspace_id": f.project.HerdrWorkspaceID, "agent_status": "idle"},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := setEnv(f.env, "HERDR_PANE_ID", f.project.LeadPaneID)
	env = setEnv(env, "HERDR_WORKSPACE_ID", f.project.HerdrWorkspaceID)
	env = setEnv(env, "HERDR_PLUGIN_EVENT_JSON", string(event))
	command := exec.Command(f.binary, "_ingest")
	command.Dir, command.Env = f.repo, env
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "ingested: true") {
		t.Fatalf("Herdr event ingest failed: %v\n%s", err, output)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Herdr event did not restore the hidden watch: %v\n%s", watchPIDs(f.root), output)
	}
}

func TestWatchRuntimeHeartbeatProgressClearsAnApparentStaleReading(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch missing: %v", watchPIDs(f.root))
	}
	pid := watchPIDs(f.root)[0]
	state, err := f.db.WatchRuntime(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	state.HeartbeatAt = time.Now().Add(-time.Hour).UnixMilli()
	if err := f.db.SetWatchRuntime(context.Background(), f.project.ID, state); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if pids := watchPIDs(f.root); len(pids) != 1 || pids[0] != pid {
		t.Fatalf("heartbeat progress unnecessarily replaced a resumed watch: %v", pids)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Watch runtime") {
			t.Fatalf("fresh heartbeat progress left a false watch failure Notice: %#v", notice)
		}
	}
}

func TestWatchRuntimeStaleHeartbeatRaisesOneNoticeAndRepairs(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch missing: %v", watchPIDs(f.root))
	}
	pid := watchPIDs(f.root)[0]
	state, err := f.db.WatchRuntime(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	initialHeartbeat := state.HeartbeatAt
	if !waitForCondition(7*time.Second, func() bool {
		state, err = f.db.WatchRuntime(context.Background(), f.project.ID)
		return err == nil && state.HeartbeatAt > initialHeartbeat
	}) {
		t.Fatal("watch did not complete a heartbeat before the stale simulation")
	}
	state.HeartbeatAt = time.Now().Add(-time.Hour).UnixMilli()
	if err := f.db.SetWatchRuntime(context.Background(), f.project.ID, state); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	resumed := false
	t.Cleanup(func() {
		if !resumed {
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
	})
	command := exec.Command(f.binary, "doctor", "--json")
	command.Dir, command.Env = f.repo, f.env
	done := make(chan struct {
		output []byte
		err    error
	}, 1)
	go func() {
		output, runErr := command.CombinedOutput()
		done <- struct {
			output []byte
			err    error
		}{output, runErr}
	}()
	if !waitForCondition(20*time.Second, func() bool {
		notices, err := f.db.Notices(context.Background(), f.project.ID, false)
		if err != nil {
			return false
		}
		for _, notice := range notices {
			if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Watch runtime") {
				return true
			}
		}
		return false
	}) {
		_ = syscall.Kill(pid, syscall.SIGCONT)
		resumed = true
		select {
		case result := <-done:
			t.Fatalf("stale watch check exited before raising its Notice: %v\n%s", result.err, result.output)
		default:
		}
		t.Fatal("stale heartbeat did not raise a watch failure Notice")
	}
	var result struct {
		output []byte
		err    error
	}
	select {
	case result = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("doctor did not finish stale watch supervision")
	}
	if result.err != nil {
		t.Fatalf("doctor failed while supervising stale watch: %v\n%s", result.err, result.output)
	}
	if pids := watchPIDs(f.root); len(pids) != 1 || pids[0] != pid {
		t.Fatalf("supervision killed or replaced a nonresponsive watch instead of stopping gracefully: %v\n%s", pids, result.output)
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	resumed = true
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("stale watch did not finish its pending graceful stop: %v", watchPIDs(f.root))
	}
	output := runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if !waitForCondition(10*time.Second, func() bool {
		pids := watchPIDs(f.root)
		return len(pids) == 1 && pids[0] != pid
	}) {
		t.Fatalf("stale heartbeat was not repaired after graceful watch exit: %v\n%s", watchPIDs(f.root), output)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Watch runtime") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("stale heartbeat created %d watch failure Notices, want one: %#v", count, notices)
	}
}

func TestConcurrentProjectCommandsStartOneWatch(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("initial Project watch missing: %v", watchPIDs(f.root))
	}
	if err := syscall.Kill(watchPIDs(f.root)[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("initial watch survived SIGKILL: %v", watchPIDs(f.root))
	}
	const commands = 8
	errors := make(chan error, commands)
	for index := 0; index < commands; index++ {
		go func() {
			command := exec.Command(f.binary, "list")
			command.Dir, command.Env = f.repo, f.env
			if output, err := command.CombinedOutput(); err != nil {
				errors <- fmt.Errorf("list command: %w: %s", err, output)
				return
			}
			errors <- nil
		}()
	}
	for index := 0; index < commands; index++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("concurrent commands did not leave exactly one watch: %v", watchPIDs(f.root))
	}
}

func TestWatchRuntimeIdentityIsStoredInProjectRuntime(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("Project watch missing: %v", watchPIDs(f.root))
	}
	state, err := f.db.WatchRuntime(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.PID != watchPIDs(f.root)[0] || state.Token == "" || state.StartedAt == 0 || state.HeartbeatAt < state.StartedAt || state.ExecutablePath == "" || state.ExecutableInode == 0 {
		t.Fatalf("persisted watch identity = %#v", state)
	}
	environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(state.PID), "environ"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"HERDR_PANE_ID=", "HERDR_WORKSPACE_ID=", "HERDR_TAB_ID="} {
		if strings.Contains(string(environ), forbidden) {
			t.Errorf("detached watch inherited %q", forbidden)
		}
	}
	if !strings.Contains(string(environ), "POSSE_WATCH_TOKEN="+state.Token) {
		t.Fatal("watch process environment does not contain its persisted token")
	}
	if panes := watchPanes(t, herdr.NewWithEnv("herdr", f.env)); len(panes) != 0 {
		t.Fatalf("watch identity test found visible Watch panes: %#v", panes)
	}
}
