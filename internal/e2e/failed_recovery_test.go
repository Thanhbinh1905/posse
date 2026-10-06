//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestFailedRecoveryIsBoundedUnderPluginEvents(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "failed-recovery")
	if _, err := f.addShipTask(t, store.StateWorking); err != nil {
		t.Fatal(err)
	}
	if err := f.attachTaskPane(t, "t1", true); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "set", "defaults.recovery_attempts", "2", "--project", f.project.Name}, {"config", "set", "defaults.recovery_backoff", "10ms", "--project", f.project.Name}, {"roster"}} {
		if code, out := f.run(t, "", args...); code != 0 {
			t.Fatalf("setup %v: %d %s", args, code, out)
		}
	}
	readTask := func() store.Task {
		t.Helper()
		var task store.Task
		if err := f.openDB(t, func(db *store.DB) error {
			var err error
			task, err = db.Task(context.Background(), f.project.ID, "t1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return task
	}
	startsBefore := 0
	workerStarts := func() int {
		h.session.mu.Lock()
		defer h.session.mu.Unlock()
		starts := 0
		for _, name := range h.session.agentStartNames {
			if strings.HasPrefix(name, "posse-"+f.project.Name+"-t1-") {
				starts++
			}
		}
		return starts - startsBefore
	}
	before := readTask()
	startsBefore = workerStarts()
	gate := make(chan struct{})
	h.session.mu.Lock()
	h.session.serverAt += "-restart"
	h.session.promptError = &herdr.APIError{Code: "agent_prompt_stalled", Message: "prompt-wait failed"}
	h.session.promptGate = gate
	h.session.mu.Unlock()
	f.env = setEnv(f.env, "HERDR_PLUGIN_EVENT_JSON", fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"working"}}`, f.project.LeadPaneID))
	command := exec.Command(h.binary, "_ingest")
	command.Dir, command.Env = f.repo, f.env
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	waitForRecoveryRequest(t, h.session.requests, "agent.prompt")
	// Read and Notice commands must not wait for the active recovery claim,
	// enter its relaunch path, or fail because its prompt-wait is failing.
	for _, args := range [][]string{{"show", "t1"}, {"ack", "all"}, {"lead"}, {"lookout", "--timeout", "10"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		read := exec.CommandContext(ctx, h.binary, args...)
		read.Dir, read.Env = f.repo, f.env
		out, err := read.CombinedOutput()
		cancel()
		if err != nil {
			t.Errorf("%v during active recovery: %v %s", args, err, out)
		}
	}
	if got := workerStarts(); got != 1 {
		t.Errorf("inspection started %d Riders, want 1", got)
	}
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ingest: %v %s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("recovery did not finish")
	}
	for i := 0; i < 12; i++ {
		code, out := f.run(t, "", "_ingest")
		if code != 0 || !strings.Contains(out, "ingested: true") {
			t.Fatalf("event %d: %d %s", i, code, out)
		}
	}
	after := readTask()
	var state store.TaskRecovery
	var notices int
	if err := f.openDB(t, func(db *store.DB) error {
		var err error
		state, err = db.TaskRecovery(context.Background(), after.ID)
		if err != nil {
			return err
		}
		return db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=?`, after.ID).Scan(&notices)
	}); err != nil {
		t.Fatal(err)
	}
	if after.Launches != before.Launches+2 || workerStarts() != 2 || state.Status != "exhausted" || state.Attempts != 2 || notices != 1 {
		t.Fatalf("failed recovery not bounded: launches %d -> %d, starts=%d state=%#v Notices=%d", before.Launches, after.Launches, workerStarts(), state, notices)
	}
	// Acknowledgement and another server generation must not re-arm exhaustion.
	for _, args := range [][]string{{"show", "t1"}, {"ack", "all"}, {"lead"}} {
		if code, out := f.run(t, "", args...); code != 0 {
			t.Fatalf("%v after exhaustion: %d %s", args, code, out)
		}
	}
	h.session.mu.Lock()
	h.session.serverAt += "-again"
	h.session.mu.Unlock()
	for i := 0; i < 5; i++ {
		if code, out := f.run(t, "", "_ingest"); code != 0 {
			t.Fatalf("late event: %d %s", code, out)
		}
	}
	if workerStarts() != 2 || readTask().Launches != after.Launches {
		t.Fatal("late events re-armed exhausted recovery")
	}
	if err := f.openDB(t, func(db *store.DB) error {
		return db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='recovery_failed'`, after.ID).Scan(&notices)
	}); err != nil || notices != 1 {
		t.Fatalf("exhaustion Notice count=%d err=%v", notices, err)
	}
	t.Logf("configured bound=2: launches=%d, Rider process groups=%d, exhaustion Notices=%d; inspection succeeded during active prompt-wait", after.Launches-before.Launches, workerStarts(), notices)
	// Explicit successful relaunch is the escape hatch, not a plugin event.
	h.session.mu.Lock()
	h.session.promptError, h.session.promptGate = nil, nil
	h.session.mu.Unlock()
	if code, out := f.run(t, "", "relaunch", "t1"); code != 0 {
		t.Fatalf("explicit relaunch: %d %s", code, out)
	}
	if err := f.openDB(t, func(db *store.DB) error {
		var err error
		state, err = db.TaskRecovery(context.Background(), after.ID)
		return err
	}); err != nil || state.Status != "recovered" || state.Attempts != 0 {
		t.Fatalf("explicit relaunch did not reset episode: %#v %v", state, err)
	}
}

func TestAutomaticRecoveryPreservesLateFailedSignal(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "late-failed")
	if _, err := f.addShipTask(t, store.StateWorking); err != nil {
		t.Fatal(err)
	}
	if err := f.attachTaskPane(t, "t1", true); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run(t, "", "roster"); code != 0 {
		t.Fatalf("setup: %d %s", code, out)
	}
	gate := make(chan struct{})
	h.session.mu.Lock()
	h.session.serverAt += "-restart"
	h.session.promptGate = gate
	h.session.mu.Unlock()
	f.env = setEnv(f.env, "HERDR_PLUGIN_EVENT_JSON", fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"working"}}`, f.project.LeadPaneID))
	command := exec.Command(h.binary, "_ingest")
	command.Dir, command.Env = f.repo, f.env
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	waitForRecoveryRequest(t, h.session.requests, "agent.prompt")
	if err := f.openDB(t, func(db *store.DB) error {
		return db.Transition(context.Background(), f.taskDBID, store.StateWorking, store.StateFailed, "worker", "Rider failed during recovery")
	}); err != nil {
		t.Fatal(err)
	}
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ingest: %v %s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("recovery did not finish")
	}
	if err := f.openDB(t, func(db *store.DB) error {
		task, err := db.Task(context.Background(), f.project.ID, "t1")
		if err == nil && task.State != store.StateFailed {
			return fmt.Errorf("automatic recovery revived a failed Rider: %s", task.State)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitRelaunchSettlesPendingRecovery(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "explicit-pending")
	if _, err := f.addShipTask(t, store.StateWorking); err != nil {
		t.Fatal(err)
	}
	if err := f.attachTaskPane(t, "t1", true); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run(t, "", "roster"); code != 0 {
		t.Fatalf("setup: %d %s", code, out)
	}
	h.session.mu.Lock()
	h.session.serverAt += "-restart"
	h.session.promptError = &herdr.APIError{Code: "agent_prompt_stalled", Message: "prompt-wait failed"}
	startsBefore := len(h.session.agentStartNames)
	h.session.mu.Unlock()
	f.env = setEnv(f.env, "HERDR_PLUGIN_EVENT_JSON", fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"working"}}`, f.project.LeadPaneID))
	if code, out := f.run(t, "", "_ingest"); code != 0 {
		t.Fatalf("recovery: %d %s", code, out)
	}
	h.session.mu.Lock()
	h.session.promptError = nil
	h.session.mu.Unlock()
	if code, out := f.run(t, "", "relaunch", "t1"); code != 0 {
		t.Fatalf("explicit relaunch: %d %s", code, out)
	}
	for i := 0; i < 3; i++ {
		if code, out := f.run(t, "", "_ingest"); code != 0 {
			t.Fatalf("late event: %d %s", code, out)
		}
	}
	h.session.mu.Lock()
	starts := 0
	for _, name := range h.session.agentStartNames[startsBefore:] {
		if strings.HasPrefix(name, "posse-"+f.project.Name+"-t1-") {
			starts++
		}
	}
	h.session.mu.Unlock()
	if starts != 2 {
		t.Fatalf("pending generation re-triggered recovery after successful explicit relaunch: starts=%d, want one failed automatic attempt plus one explicit launch", starts)
	}
}

// A second group close can interrupt the first Lead's opening prompt. A
// waiting hook may then restore the Lead from a snapshot containing Riders
// that the close subsequently removes. The completed Project claim must not
// hide those missing Riders from the older group episode.
func TestRecoveryRepairsRidersFromAnInterruptedGroupEpisode(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "partial-group-recovery")
	if _, err := f.addShipTask(t, store.StateWorking); err != nil {
		t.Fatal(err)
	}
	if err := f.attachTaskPane(t, "t1", true); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run(t, "", "roster"); code != 0 {
		t.Fatalf("initial reconcile: %d %s", code, out)
	}
	var before store.Task
	if err := f.openDB(t, func(db *store.DB) error {
		var err error
		before, err = db.Task(context.Background(), f.project.ID, "t1")
		if err != nil {
			return err
		}
		claimed, err := db.ClaimTaskRecovery(context.Background(), before.ID, h.session.serverAt+"/group/old-lead/old-pane", 0, 101, 3, 1, 2)
		if err != nil || !claimed {
			return fmt.Errorf("seed group claim: claimed=%v err=%v", claimed, err)
		}
		if err := db.FinishTaskRecovery(context.Background(), before, 101, 3, true, "", 0); err != nil {
			return err
		}
		_, err = db.ExecContext(context.Background(), `UPDATE project_runtime SET recovery_generation=? WHERE project_id=?`, h.session.serverAt+"/group/new-lead/", f.project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.session.mu.Lock()
	h.session.forgetAgent(before.PaneID)
	delete(h.session.panes, before.PaneID)
	delete(h.session.workspaces, before.HerdrWorkspaceID)
	h.session.mu.Unlock()
	code, out := f.run(t, "", "recover", "--all")
	t.Logf("group repair: %d %s", code, out)
	if code != 0 {
		t.Fatalf("repair: %d %s", code, out)
	}
	if err := f.openDB(t, func(db *store.DB) error {
		after, err := db.Task(context.Background(), f.project.ID, "t1")
		if err != nil {
			return err
		}
		state, _ := db.TaskRecovery(context.Background(), before.ID)
		t.Logf("repaired group Rider: %#v", state)
		if after.Launches != before.Launches+1 || after.State != store.StateWorking || after.MountID != before.MountID || after.Branch != before.Branch {
			return fmt.Errorf("missing group Rider was not repaired: before=%#v after=%#v", before, after)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRetriesMissingRiderMarkedRecoveredInSameGroupEpisode(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "recovered-group-rider-missing")
	if _, err := f.addShipTask(t, store.StateWorking); err != nil {
		t.Fatal(err)
	}
	if err := f.attachTaskPane(t, "t1", true); err != nil {
		t.Fatal(err)
	}
	if code, out := f.run(t, "", "roster"); code != 0 {
		t.Fatalf("initial reconcile: %d %s", code, out)
	}
	readTask := func() store.Task {
		t.Helper()
		var task store.Task
		if err := f.openDB(t, func(db *store.DB) error {
			var err error
			task, err = db.Task(context.Background(), f.project.ID, "t1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return task
	}
	before := readTask()
	generation := h.session.serverAt + "/group/" + f.project.HerdrWorkspaceID + "/" + f.project.LeadPaneID
	if err := f.openDB(t, func(db *store.DB) error {
		claimed, err := db.ClaimTaskRecovery(context.Background(), before.ID, generation, 0, 101, 3, 1000, 2000)
		if err != nil || !claimed {
			return fmt.Errorf("seed completed group recovery claim: claimed=%v err=%v", claimed, err)
		}
		return db.FinishTaskRecovery(context.Background(), before, 101, 3, true, "", 2000)
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a second group close before another hook has reconciled the
	// completed recovery. The server generation and group recovery key remain
	// unchanged, but the Rider pane is gone.
	h.session.mu.Lock()
	for paneID, pane := range h.session.panes {
		if pane.WorkspaceID == f.project.HerdrWorkspaceID || pane.Label == before.PaneLabel {
			delete(h.session.panes, paneID)
			h.session.forgetAgent(paneID)
		}
	}
	for workspaceID, workspace := range h.session.workspaces {
		if workspaceID == f.project.HerdrWorkspaceID || workspace.Worktree.CheckoutPath == before.WorktreePath {
			delete(h.session.workspaces, workspaceID)
		}
	}
	h.session.mu.Unlock()
	if code, out := f.run(t, "", "recover", "--all"); code != 0 {
		t.Fatalf("same-generation group recovery: %d %s", code, out)
	}
	after := readTask()
	var state store.TaskRecovery
	if err := f.openDB(t, func(db *store.DB) error {
		var err error
		state, err = db.TaskRecovery(context.Background(), after.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.Launches != before.Launches+1 || after.State != store.StateWorking || state.Status != "recovered" || state.Attempts != 2 {
		t.Fatalf("missing Rider was treated as already recovered: launches %d -> %d, task=%s, recovery=%#v", before.Launches, after.Launches, after.State, state)
	}
	snapshot, err := h.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pane, found := herdr.FindPane(snapshot.Panes, after.PaneID, after.PaneLabel)
	if !found || pane.Agent == "" || pane.AgentStatus == "exited" || pane.AgentStatus == "stopped" {
		t.Fatalf("recovery record says recovered but Rider pane is not live: state=%#v pane=%#v found=%v", state, pane, found)
	}
}

func TestRecoveryCrashConsumesFinalAttempt(t *testing.T) {
	h := newIntentCLIHarness(t)
	f := h.newProject(t, "recovery-crash-budget")
	if _, err := f.addShipTask(t, store.StateWorking); err != nil {
		t.Fatal(err)
	}
	if err := f.attachTaskPane(t, "t1", true); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "set", "defaults.recovery_attempts", "1", "--project", f.project.Name}, {"roster"}} {
		if code, out := f.run(t, "", args...); code != 0 {
			t.Fatalf("setup: %d %s", code, out)
		}
	}
	h.session.mu.Lock()
	h.session.serverAt += "-restart"
	startsBefore := len(h.session.agentStartNames)
	h.session.mu.Unlock()
	f.env = setEnv(f.env, "HERDR_PLUGIN_EVENT_JSON", fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"working"}}`, f.project.LeadPaneID))
	if code, out := f.run(t, "relaunch:after:agent.start", "_ingest"); code != 86 {
		t.Fatalf("recovery did not crash after spawn: %d %s", code, out)
	}
	for i := 0; i < 5; i++ {
		if code, out := f.run(t, "", "_ingest"); code != 0 {
			t.Fatalf("late event: %d %s", code, out)
		}
	}
	var state store.TaskRecovery
	var notices int
	if err := f.openDB(t, func(db *store.DB) error {
		var err error
		state, err = db.TaskRecovery(context.Background(), f.taskDBID)
		if err != nil {
			return err
		}
		return db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='recovery_failed'`, f.taskDBID).Scan(&notices)
	}); err != nil {
		t.Fatal(err)
	}
	h.session.mu.Lock()
	starts := 0
	for _, name := range h.session.agentStartNames[startsBefore:] {
		if strings.HasPrefix(name, "posse-"+f.project.Name+"-t1-") {
			starts++
		}
	}
	h.session.mu.Unlock()
	if starts != 1 || state.Attempts != 1 || state.Status != "exhausted" || notices != 1 {
		t.Fatalf("crash bypassed budget: starts=%d state=%#v Notices=%d", starts, state, notices)
	}
}
