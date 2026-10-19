//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
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

func lookoutPanes(t *testing.T, client *herdr.Client) []herdr.Pane {
	t.Helper()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var panes []herdr.Pane
	for _, pane := range snapshot.Panes {
		if pane.Label == "posse:shop:lookout" {
			panes = append(panes, pane)
		}
	}
	return panes
}

// lookoutPIDs lists hidden poll-only Lookout processes that belong to this fixture.
func lookoutPIDs(root string) []int {
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
				continue // An exited zombie is not a running Lookout.
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

func stopFixtureLookoutPollers(root string) {
	pids := lookoutPIDs(root)
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if waitForCondition(3*time.Second, func() bool { return len(lookoutPIDs(root)) == 0 }) {
		return
	}
	for _, pid := range lookoutPIDs(root) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = waitForCondition(2*time.Second, func() bool { return len(lookoutPIDs(root)) == 0 })
}

func TestLookoutRestartsAfterProcessExit(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Lookout process not running: %v", lookoutPIDs(f.root))
	}
	before := lookoutPIDs(f.root)[0]
	if err := syscall.Kill(before, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatalf("killed Lookout process remains visible: %v", lookoutPIDs(f.root))
	}
	output := runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if !strings.Contains(output, `"check":"Project Lookout shop"`) {
		t.Fatalf("doctor omitted poller health: %s", output)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("the next Project command did not repair the poller: %v\n%s", lookoutPIDs(f.root), output)
	}
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("hidden poller opened %d visible Lookout panes: %#v", len(panes), panes)
	}
}

func TestLookoutDoesNotOpenLoginShellOrVisiblePane(t *testing.T) {
	f := newPRLifecycleFixtureWithSlowLoginShell(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Project poller did not start: %v", lookoutPIDs(f.root))
	}
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("Lookout opened visible panes: %#v", panes)
	}
	if starts := lineCount(filepath.Join(f.root, "slow-login-starts")); starts != 0 {
		t.Fatalf("hidden poller started %d login shells", starts)
	}
}

func lineCount(path string) int {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(contents), "started\n")
}

func TestHiddenLookoutPollerDeliversNoticesWhenLeadHasNoWait(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Project poller did not start: %v", lookoutPIDs(f.root))
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
		t.Fatalf("hidden poller did not deliver a Notice without a Lead-side wait: %#v", notices)
	}
	contents, err := os.ReadFile(filepath.Join(f.root, "lead-prompts.log"))
	if err != nil || !strings.Contains(string(contents), "Worker finished") {
		t.Fatalf("hidden poller did not wake the idle Lead: %s (%v)", contents, err)
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

func TestLookoutRestartsAfterHerdrRestart(t *testing.T) {
	checkLookoutRestore(t, false)
}

func TestLookoutRestartsWithoutRecordedGeneration(t *testing.T) {
	checkLookoutRestore(t, true)
}

func checkLookoutRestore(t *testing.T, clearGeneration bool) {
	t.Helper()
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatal("hidden poller missing before Herdr restart")
	}
	if len(lookoutPanes(t, client)) != 0 {
		t.Fatal("Project has a visible Lookout pane before Herdr restart")
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
	if !waitForCondition(15*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden poller missing after Herdr restart: %v\n%s", lookoutPIDs(f.root), output)
	}
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("Herdr restart restored %d visible Lookout panes: %#v", len(panes), panes)
	}
}

func TestConcurrentLookoutTeardownRaisesNoFalseReason(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "lookout-follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	landing := f.mustTask(t, "t1")
	merge := f.mergeOnLocalOrigin(t, landing, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
	showAfterActiveTeardown(t, f, "t1")
	assertNoValidationFixRound(t, f, "t1", "gate")
	assertNoValidationFixRound(t, f, "t1", "freshness")
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "unsaddle_incomplete" {
			t.Errorf("state=%s false reason: %s", f.mustTask(t, "t1").State, notice.Summary)
		}
	}
}

func TestUpClosesLegacyVisibleLookoutTab(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if _, err := client.Call(context.Background(), "tab.create", map[string]any{
		"workspace_id": f.project.HerdrWorkspaceID, "cwd": f.repo, "label": "posse:shop:lookout", "focus": false,
	}); err != nil {
		t.Fatalf("create legacy Lookout tab: %v", err)
	}
	command := exec.Command(f.binary, "up", "--name", "shop")
	command.Dir, command.Env = f.repo, f.leadEnv
	_, _ = command.CombinedOutput() // A live Lead may make `up` return lead_running after migration.
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("legacy Lookout tab survived posse up: %#v", panes)
	}
}

func TestLookoutPollerReexecsAfterBinaryReplacement(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("Project poller missing before update simulation: %v", lookoutPIDs(f.root))
	}
	pid := lookoutPIDs(f.root)[0]
	before, err := f.db.LookoutPoller(context.Background(), f.project.ID)
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
		state, stateErr := f.db.LookoutPoller(context.Background(), f.project.ID)
		return stateErr == nil && state.PID == pid && state.ExecutableInode == executable.Ino && len(lookoutPIDs(f.root)) == 1
	}) {
		state, _ := f.db.LookoutPoller(context.Background(), f.project.ID)
		t.Fatalf("poller did not re-exec into the replacement binary: before=%#v after=%#v pids=%v", before, state, lookoutPIDs(f.root))
	}
}

func TestLookoutRepairsAfterHerdrAgentEvent(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("initial Project poller missing: %v", lookoutPIDs(f.root))
	}
	if err := syscall.Kill(lookoutPIDs(f.root)[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatalf("killed poller remains: %v", lookoutPIDs(f.root))
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
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("Herdr event did not restore the hidden poller: %v\n%s", lookoutPIDs(f.root), output)
	}
}

func TestLookoutPollerHeartbeatProgressClearsAnApparentStaleReading(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("Project poller missing: %v", lookoutPIDs(f.root))
	}
	pid := lookoutPIDs(f.root)[0]
	state, err := f.db.LookoutPoller(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	state.HeartbeatAt = time.Now().Add(-time.Hour).UnixMilli()
	if err := f.db.SetLookoutPoller(context.Background(), f.project.ID, state); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if pids := lookoutPIDs(f.root); len(pids) != 1 || pids[0] != pid {
		t.Fatalf("heartbeat progress unnecessarily replaced a resumed poller: %v", pids)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Lookout poller") {
			t.Fatalf("fresh heartbeat progress left a false poller failure Notice: %#v", notice)
		}
	}
}

func TestLookoutPollerStaleHeartbeatRaisesOneNoticeAndRepairs(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("Project poller missing: %v", lookoutPIDs(f.root))
	}
	pid := lookoutPIDs(f.root)[0]
	state, err := f.db.LookoutPoller(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	initialHeartbeat := state.HeartbeatAt
	if !waitForCondition(7*time.Second, func() bool {
		state, err = f.db.LookoutPoller(context.Background(), f.project.ID)
		return err == nil && state.HeartbeatAt > initialHeartbeat
	}) {
		t.Fatal("poller did not complete a heartbeat before the stale simulation")
	}
	state.HeartbeatAt = time.Now().Add(-time.Hour).UnixMilli()
	if err := f.db.SetLookoutPoller(context.Background(), f.project.ID, state); err != nil {
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
			if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Lookout poller") {
				return true
			}
		}
		return false
	}) {
		_ = syscall.Kill(pid, syscall.SIGCONT)
		resumed = true
		select {
		case result := <-done:
			t.Fatalf("stale poller check exited before raising its Notice: %v\n%s", result.err, result.output)
		default:
		}
		t.Fatal("stale heartbeat did not raise a poller failure Notice")
	}
	var result struct {
		output []byte
		err    error
	}
	select {
	case result = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("doctor did not finish stale poller supervision")
	}
	if result.err != nil {
		t.Fatalf("doctor failed while supervising stale poller: %v\n%s", result.err, result.output)
	}
	if pids := lookoutPIDs(f.root); len(pids) != 1 || pids[0] != pid {
		t.Fatalf("supervision killed or replaced a nonresponsive poller instead of stopping gracefully: %v\n%s", pids, result.output)
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	resumed = true
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatalf("stale poller did not finish its pending graceful stop: %v", lookoutPIDs(f.root))
	}
	output := runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if !waitForCondition(10*time.Second, func() bool {
		pids := lookoutPIDs(f.root)
		return len(pids) == 1 && pids[0] != pid
	}) {
		t.Fatalf("stale heartbeat was not repaired after graceful poller exit: %v\n%s", lookoutPIDs(f.root), output)
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Lookout poller") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("stale heartbeat created %d poller failure Notices, want one: %#v", count, notices)
	}
}

func TestConcurrentProjectCommandsStartOnePoller(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("initial Project poller missing: %v", lookoutPIDs(f.root))
	}
	if err := syscall.Kill(lookoutPIDs(f.root)[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatalf("initial poller survived SIGKILL: %v", lookoutPIDs(f.root))
	}
	const commands = 8
	errors := make(chan error, commands)
	for index := 0; index < commands; index++ {
		go func() {
			command := exec.Command(f.binary, "roster")
			command.Dir, command.Env = f.repo, f.env
			if output, err := command.CombinedOutput(); err != nil {
				errors <- fmt.Errorf("roster command: %w: %s", err, output)
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
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("concurrent commands did not leave exactly one poller: %v", lookoutPIDs(f.root))
	}
}

func TestLookoutPollerIdentityIsStoredInProjectRuntime(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("Project poller missing: %v", lookoutPIDs(f.root))
	}
	state, err := f.db.LookoutPoller(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.PID != lookoutPIDs(f.root)[0] || state.Token == "" || state.StartedAt == 0 || state.HeartbeatAt < state.StartedAt || state.ExecutablePath == "" || state.ExecutableInode == 0 {
		t.Fatalf("persisted poller identity = %#v", state)
	}
	environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(state.PID), "environ"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"HERDR_PANE_ID=", "HERDR_WORKSPACE_ID=", "HERDR_TAB_ID="} {
		if strings.Contains(string(environ), forbidden) {
			t.Errorf("detached poller inherited %q", forbidden)
		}
	}
	if !strings.Contains(string(environ), "POSSE_LOOKOUT_TOKEN="+state.Token) {
		t.Fatal("poller process environment does not contain its persisted token")
	}
	if panes := lookoutPanes(t, herdr.NewWithEnv("herdr", f.env)); len(panes) != 0 {
		t.Fatalf("poller identity test found visible Lookout panes: %#v", panes)
	}
}
