//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestFailedGroupAttemptRetriesAfterLeadStarts(t *testing.T) {
	for _, trigger := range []string{"plugin-event", "recover-all"} {
		t.Run(trigger, func(t *testing.T) { runFailedGroupAttemptRetriesAfterLeadStarts(t, trigger) })
	}
}

func runFailedGroupAttemptRetriesAfterLeadStarts(t *testing.T, trigger string) {
	t.Helper()
	f := newRiderTabsFixture(t)
	t.Cleanup(func() { stopIsolatedFinalizers(t, f.root) })
	startsLog := filepath.Join(f.root, "failed-group-starts.log")
	failPrompts := filepath.Join(f.root, "fail-recovery-prompts")
	agentPath := filepath.Join(f.root, "bin", "claude")
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	instrument := `case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*) printf 'worker\n' >> "$POSSE_TEST_ROOT/failed-group-starts.log" ;;
  *) printf 'lead\n' >> "$POSSE_TEST_ROOT/failed-group-starts.log" ;;
esac
`
	// Fail only automatic recovery's prompt delivery. The initial Rider and
	// later retry use the healthy harness.
	script := strings.Replace(string(agent), "#!/bin/sh\n", "#!/bin/sh\n"+instrument, 1)
	script = strings.Replace(script,
		"    IFS= read -r prompt || exit 0\n",
		"    IFS= read -r prompt || exit 0\n    if [ -f \"$POSSE_TEST_ROOT/fail-recovery-prompts\" ]; then\n      while IFS= read -r line; do :; done\n      exit 0\n    fi\n", 1)
	if err := os.WriteFile(agentPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	workerStarts := func() int {
		t.Helper()
		data, err := os.ReadFile(startsLog)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Count(string(data), "worker\n")
	}
	leadStarts := func() int {
		t.Helper()
		data, err := os.ReadFile(startsLog)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return strings.Count(string(data), "lead\n")
	}
	readRecovery := func() store.TaskRecovery {
		t.Helper()
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		task := f.task(t, "t1")
		state, err := db.TaskRecovery(context.Background(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	before := f.ride(t, "t1", "Real failed recovery", "real-failed-recovery")
	if got := workerStarts(); got != 1 {
		t.Fatalf("initial Rider starts=%d, want 1", got)
	}
	preservedPath := filepath.Join(before.WorktreePath, "review-held-work.txt")
	if err := os.WriteFile(preservedPath, []byte("uncommitted Rider work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	branchBefore := gitTest(t, f.env, f.repo, "rev-parse", before.Branch)
	runPosse(t, f.binary, f.repo, f.leadEnv, "config", "set", "defaults.recovery_backoff", "15s", "--project", "shop")
	if err := os.WriteFile(failPrompts, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	closeRiderGroup(t, f)
	output := runPosse(t, f.binary, f.repo, f.env, "recover", "--all")
	state := readRecovery()
	if state.Status != "pending" || state.Attempts != 1 || state.NextAttemptAt <= time.Now().UnixMilli() {
		t.Fatalf("did not produce a real pending backoff: %#v output=%s", state, output)
	}
	retryAt := state.NextAttemptAt
	if !waitForCondition(2*time.Second, func() bool { return workerStarts() == 2 }) {
		t.Fatalf("failed attempt process did not start: starts=%d", workerStarts())
	}
	if got := workerStarts(); got != 2 {
		t.Fatalf("failed automatic attempt starts=%d, want original plus failed attempt", got)
	}
	if err := os.Remove(failPrompts); err != nil {
		t.Fatal(err)
	}

	caller, err := createWorkspace(f.client, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Run(context.Background(), "pane", "run", caller.RootPane.PaneID, "posse up --name shop --yes"); err != nil {
		t.Fatalf("run replacement posse up in isolated Herdr pane: %v", err)
	}
	if !waitForCondition(6*time.Second, func() bool {
		for _, pane := range f.snapshot(t).Panes {
			if pane.PaneID == caller.RootPane.PaneID && pane.Agent != "" && pane.AgentStatus != "stopped" && pane.AgentStatus != "exited" {
				return true
			}
		}
		return false
	}) {
		pane, _ := f.client.Run(context.Background(), "pane", "read", caller.RootPane.PaneID, "--source", "recent-unwrapped", "--lines", "50")
		t.Fatalf("replacement Lead did not start during backoff: %s", pane)
	}
	if !waitForCondition(2*time.Second, func() bool { return leadStarts() == 1 }) {
		t.Fatalf("replacement Lead starts=%d, want exactly one", leadStarts())
	}
	lead := herdr.Pane{PaneID: caller.RootPane.PaneID, TabID: caller.RootPane.TabID, WorkspaceID: caller.Workspace.WorkspaceID}
	f.leadEnv = setEnv(f.env, "HERDR_ENV", "1")
	f.leadEnv = setEnv(f.leadEnv, "HERDR_PANE_ID", lead.PaneID)
	f.leadEnv = setEnv(f.leadEnv, "HERDR_TAB_ID", lead.TabID)
	f.leadEnv = setEnv(f.leadEnv, "HERDR_WORKSPACE_ID", lead.WorkspaceID)
	healthy := f.ride(t, "t2", "Healthy new Rider", "healthy-new-rider")
	if healthy.Launches != 1 || healthy.State != store.StateWorking || healthy.AgentServerStartedAt != f.snapshot(t).ServerStartedAt || time.Now().UnixMilli() >= retryAt {
		t.Fatalf("new Rider did not start healthy in the current generation during backoff: %#v", healthy)
	}
	state = readRecovery()
	if state.Status != "pending" || state.Attempts != 1 || state.NextAttemptAt != retryAt {
		t.Fatalf("early up changed the pending retry episode: %#v, expected retry_at=%d", state, retryAt)
	}

	if delay := time.Until(time.UnixMilli(state.NextAttemptAt)); delay > 0 {
		time.Sleep(delay + 250*time.Millisecond)
	}
	pluginEnv := setEnv(f.env, "HERDR_PLUGIN_EVENT_JSON", fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"working"}}`, lead.PaneID))
	if trigger == "plugin-event" {
		for range 4 {
			runPosse(t, f.binary, f.repo, pluginEnv, "_ingest")
		}
	} else {
		output = runPosse(t, f.binary, f.repo, f.env, "recover", "--all")
	}
	current := f.task(t, "t1")
	state = readRecovery()
	if current.State != store.StateWorking || current.Launches != before.Launches+2 || state.Status != "recovered" || state.Attempts != 2 || workerStarts() < 4 || leadStarts() != 1 {
		t.Fatalf("%s did not recover the pending Rider once after backoff: before=%#v current=%#v recovery=%#v worker_starts=%d lead_starts=%d", trigger, before, current, state, workerStarts(), leadStarts())
	}
	if currentHealthy := f.task(t, "t2"); currentHealthy.Launches != healthy.Launches || currentHealthy.AgentName != healthy.AgentName || workerStarts() != 4 {
		t.Fatalf("deferred group recovery restarted healthy new Rider: before=%#v after=%#v worker_starts=%d", healthy, currentHealthy, workerStarts())
	}
	healthySnapshot := f.snapshot(t)
	healthyPane, found := herdr.FindPane(healthySnapshot.Panes, healthy.PaneID, healthy.PaneLabel)
	if !found || healthyPane.Agent == "" || healthyPane.AgentStatus == "exited" || healthyPane.AgentStatus == "stopped" {
		t.Fatalf("deferred group recovery stopped healthy Rider agent: pane=%#v found=%v", healthyPane, found)
	}
	if trigger == "plugin-event" {
		output = runPosse(t, f.binary, f.repo, f.env, "recover", "--all")
	} else {
		for range 4 {
			runPosse(t, f.binary, f.repo, pluginEnv, "_ingest")
		}
	}
	if after := f.task(t, "t1"); after.Launches != current.Launches || workerStarts() != 4 {
		t.Fatalf("second recovery trigger duplicated the completed retry: trigger=%s current=%#v after=%#v starts=%d output=%s", trigger, current, after, workerStarts(), output)
	}
	if afterHealthy := f.task(t, "t2"); afterHealthy.Launches != healthy.Launches || afterHealthy.AgentName != healthy.AgentName {
		t.Fatalf("second recovery trigger restarted healthy new Rider: before=%#v after=%#v", healthy, afterHealthy)
	}
	if current.MountID != before.MountID || current.Branch != before.Branch {
		t.Fatalf("retry changed held work identity: before=%#v current=%#v", before, current)
	}
	work, err := os.ReadFile(preservedPath)
	if err != nil || string(work) != "uncommitted Rider work\n" || gitTest(t, f.env, f.repo, "rev-parse", before.Branch) != branchBefore {
		t.Fatalf("retry did not preserve Rider work: file=%q err=%v", work, err)
	}
	if locks := gitTest(t, f.env, f.repo, "worktree", "list", "--porcelain"); !strings.Contains(locks, "locked posse: held by t1") {
		t.Fatalf("Mount lock lost after retry: %s", locks)
	}
}
