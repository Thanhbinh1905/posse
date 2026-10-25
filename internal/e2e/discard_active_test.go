//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestDoneTaskWithVanishedPaneCanBeDiscarded(t *testing.T) {
	f := newWorkerTabsFixture(t)
	completeAgent := `#!/bin/sh
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    : > "$POSSE_TEST_ROOT/agent-ready"
    while [ ! -e "$POSSE_TEST_ROOT/agent-finished" ]; do sleep 0.02; done
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    ;;
  *)
    while IFS= read -r line; do :; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(f.root, "bin", "claude"), []byte(completeAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	task := f.start(t, "t1", "Discard completed Worker", "discard-completed-worker")
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "agent-ready"))
		return err == nil
	}) {
		t.Fatal("fixture Worker agent did not become ready")
	}
	change := filepath.Join(task.WorktreePath, "completed-change.txt")
	if err := os.WriteFile(change, []byte("committed Worker change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, task.WorktreePath, "add", "completed-change.txt")
	gitTest(t, f.env, task.WorktreePath, "commit", "-m", "complete Worker change")

	workerEnv := setEnv(f.env, "HERDR_ENV", "1")
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", task.PaneID)
	workerEnv = setEnv(workerEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	workerEnv = setEnv(workerEnv, "HERDR_TAB_ID", tabOf(t, f.snapshot(t), task.PaneID))
	done := exec.Command(f.binary, "signal", "done", "Worker work is complete")
	done.Dir, done.Env = task.WorktreePath, workerEnv
	if output, err := done.CombinedOutput(); err != nil {
		t.Fatalf("Worker completion Signal: %v\n%s", err, output)
	}
	before := f.task(t, "t1")
	if before.State != store.StateDone {
		t.Fatalf("Worker Signal left Task in %s", before.State)
	}
	branchTip := strings.TrimSpace(gitTest(t, f.env, f.repo, "rev-parse", "refs/heads/"+task.Branch))
	if err := os.WriteFile(filepath.Join(f.root, "agent-finished"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		raw, err := f.client.Call(context.Background(), "pane.process_info", map[string]any{"pane_id": task.PaneID})
		if err != nil {
			return false
		}
		var info struct {
			ProcessInfo struct {
				ShellPID               int `json:"shell_pid"`
				ForegroundProcessGroup int `json:"foreground_process_group_id"`
			} `json:"process_info"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			return false
		}
		return info.ProcessInfo.ShellPID > 1 && info.ProcessInfo.ForegroundProcessGroup == info.ProcessInfo.ShellPID
	}) {
		t.Fatal("fixture Worker agent did not exit after completing its work")
	}

	oldGeneration, err := f.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	killServer(t, f.server)
	f.server = startServer(t, f.client)
	restored, err := f.client.Snapshot(context.Background())
	if err != nil || restored.ServerStartedAt == oldGeneration.ServerStartedAt {
		t.Fatalf("isolated Herdr did not establish a new generation: before=%q after=%q err=%v", oldGeneration.ServerStartedAt, restored.ServerStartedAt, err)
	}
	for _, pane := range restored.Panes {
		if pane.Label == task.PaneLabel {
			if _, err := f.client.Call(context.Background(), "tab.close", map[string]any{"tab_id": pane.TabID}); err != nil {
				t.Fatalf("remove completed Worker pane after Herdr restart: %v", err)
			}
			break
		}
	}
	runPosse(t, f.binary, f.repo, f.env, "list")
	snapshot := f.snapshot(t)
	lead, found := findLeadInSnapshot(snapshot)
	if !found {
		t.Fatal("Lead pane disappeared after Herdr restart")
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByName(context.Background(), "shop")
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if project.LeadPaneID != lead.PaneID {
		t.Fatalf("Project Lead pane %q does not match restored pane %q", project.LeadPaneID, lead.PaneID)
	}
	f.leadPaneID = project.LeadPaneID
	f.leadEnv = setEnv(f.env, "HERDR_ENV", "1")
	f.leadEnv = setEnv(f.leadEnv, "HERDR_PANE_ID", project.LeadPaneID)
	f.leadEnv = setEnv(f.leadEnv, "HERDR_WORKSPACE_ID", project.HerdrWorkspaceID)
	f.leadEnv = setEnv(f.leadEnv, "HERDR_TAB_ID", lead.TabID)
	current := f.task(t, "t1")
	if current.State != store.StateDone || current.Launches != before.Launches {
		t.Fatalf("vanished done Worker changed state or relaunched: before=%#v after=%#v", before, current)
	}

	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	workerExited := 0
	for _, notice := range notices {
		if notice.Kind == "worker_exited" && notice.TaskID == task.ID {
			workerExited++
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if workerExited != 1 {
		t.Fatalf("worker_exited Notice count = %d, want one", workerExited)
	}

	decisionOutput := runPosse(t, f.binary, f.repo, f.leadEnv, "decisions")
	if !strings.Contains(decisionOutput, "kind: worker_exited") || !strings.Contains(decisionOutput, "relaunch") || !strings.Contains(decisionOutput, "discard") {
		t.Fatalf("worker_exited did not offer a relaunch/discard Decision: %s", decisionOutput)
	}
	decisionDB, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := decisionDB.Decisions(context.Background(), f.projectID, true)
	if closeErr := decisionDB.Close(); err != nil || closeErr != nil {
		t.Fatalf("read worker_exited Decision: %v (close: %v)", err, closeErr)
	}
	if len(decisions) != 1 || decisions[0].Kind != "worker_exited" {
		t.Fatalf("pending worker_exited Decisions = %#v", decisions)
	}
	decision := decisions[0]
	const decisionQuote = "User approved discarding this completed Task"
	runPosse(t, f.binary, f.repo, f.leadEnv, "decide", fmt.Sprint(decision.ID), "discard", "--user-approved", decisionQuote)
	crashAt := "teardown:after:approval.record"
	interrupted := exec.Command(f.binary, "apply", fmt.Sprint(decision.ID))
	interrupted.Dir, interrupted.Env = f.repo, setEnv(f.leadEnv, "POSSE_INTENT_CRASH_AT", crashAt)
	interruptedOutput, interruptedErr := interrupted.CombinedOutput()
	exit, ok := interruptedErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("approved discard did not interrupt after recording approval: err=%v output=%s", interruptedErr, interruptedOutput)
	}
	assertCrashExit(t, exit.ExitCode(), string(interruptedOutput), crashAt)
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "apply", fmt.Sprint(decision.ID))
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("applying worker_exited discard did not report teardown: %s", output)
	}
	if current := f.task(t, "t1"); current.State != store.StateTornDown {
		t.Fatalf("approved discard left Task in %s", current.State)
	}

	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	transitions, err := db.TaskTransitions(context.Background(), task.ID, 20)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if len(transitions) < 2 || transitions[0].From != string(store.StateDone) || transitions[0].To != string(store.StateTornDown) || transitions[0].Source != "user" {
		_ = db.Close()
		t.Fatalf("latest discard transition = %#v, want direct done -> torn-down from user", transitions)
	}
	var approvals int
	var quote, approvedTip string
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*),MAX(user_quote),MAX(branch_sha) FROM approvals WHERE task_id=? AND action='discard'`, task.ID).Scan(&approvals, &quote, &approvedTip); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var releasedSandbox store.Sandbox
	for _, candidate := range sandboxes {
		if candidate.ID == task.SandboxID {
			releasedSandbox = candidate
			break
		}
	}
	_ = db.Close()
	if approvals != 1 || quote != "User approved discarding this completed Task" || approvedTip != branchTip {
		t.Fatalf("discard approval = %d, quote %q, tip %q; want one verbatim approval of %s", approvals, quote, approvedTip, branchTip)
	}
	if releasedSandbox.ID == 0 || releasedSandbox.State == "held" || releasedSandbox.TaskID != 0 {
		t.Fatalf("discard did not release Sandbox %d: %#v", task.SandboxID, releasedSandbox)
	}
	bundles, err := filepath.Glob(filepath.Join(f.home, "projects", "shop", "tasks", "t1", "discard", "*.bundle"))
	if err != nil || len(bundles) != 1 {
		t.Fatalf("discard tip bundle paths = %v, %v", bundles, err)
	}
	lead, found = findLeadInSnapshot(f.snapshot(t))
	if !found {
		t.Fatal("Lead pane disappeared during discard")
	}
	f.assertAlive(t, f.snapshot(t), lead.PaneID)

	beforeRepeat := transitions[0].ID
	runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1")
	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	afterRepeat, err := db.TaskTransitions(context.Background(), task.ID, 20)
	if err == nil && (len(afterRepeat) == 0 || afterRepeat[0].ID != beforeRepeat) {
		err = fmt.Errorf("repeat teardown recorded another transition: %#v", afterRepeat)
	}
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestApprovedDiscardStopsWorkingWorker(t *testing.T) {
	for _, state := range []store.State{store.StateWorking, store.StateNeedsDecision} {
		t.Run(string(state), func(t *testing.T) { testApprovedDiscardStopsWorker(t, state) })
	}
}

func TestApprovedDiscardStopsReopenedDoneWorker(t *testing.T) {
	testApprovedDiscardStopsWorker(t, store.StateDone)
}

func testApprovedDiscardStopsWorker(t *testing.T, state store.State) {
	f := newWorkerTabsFixture(t)
	agent := `#!/bin/sh
trap ': > "$POSSE_TEST_ROOT/worker-interrupted"; printf "late Worker commit\n" > cancel-late.txt; git add cancel-late.txt; git commit -m "late Worker commit" > "$POSSE_TEST_ROOT/late-commit.log" 2>&1' INT
trap ': > "$POSSE_TEST_ROOT/worker-terminated"; posse signal failed "Worker stopped during approved cancellation" > "$POSSE_TEST_ROOT/stop-signal.log" 2>&1; exit 0' TERM
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
IFS= read -r prompt || exit 0
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
while :; do
  IFS= read -r line || :
  if [ "$line" = 'Resume cancelled Task' ]; then
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
  fi
done
`
	if err := os.WriteFile(filepath.Join(f.root, "bin", "claude"), []byte(agent), 0o700); err != nil {
		t.Fatal(err)
	}
	task := f.start(t, "t1", "Cancel working Worker", "cancel-working-worker")
	pid := foregroundPID(t, f, task.PaneID)
	workerEnv := setEnv(f.env, "HERDR_ENV", "1")
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", task.PaneID)
	if state == store.StateNeedsDecision {
		runPosse(t, f.binary, task.WorktreePath, workerEnv, "signal", "needs-decision", "Should this Task be cancelled?")
	}
	if state == store.StateDone {
		if err := os.WriteFile(filepath.Join(task.WorktreePath, "completed-change.txt"), []byte("completed Worker work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitTest(t, f.env, task.WorktreePath, "add", "completed-change.txt")
		gitTest(t, f.env, task.WorktreePath, "commit", "-m", "complete Worker change")
		if _, err := f.client.Run(t.Context(), "pane", "report-agent", task.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
			t.Fatal(err)
		}
		runPosse(t, f.binary, task.WorktreePath, workerEnv, "signal", "done", "Worker work is complete")
		if completed := f.task(t, "t1"); completed.State != store.StateDone {
			t.Fatalf("Worker completion left Task in %s", completed.State)
		}
		// Direct User input, not posse send or a working Signal, must reopen
		// this Change Task before the Lead cancels that same Worker instance.
		if _, err := f.client.Run(t.Context(), "pane", "run", task.PaneID, "Resume cancelled Task"); err != nil {
			t.Fatal(err)
		}
		// The resumed turn changes the repository, which is what reopens the Task (#443).
		if err := os.WriteFile(filepath.Join(task.WorktreePath, "resumed-change.txt"), []byte("resumed Worker work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(30*time.Second, func() bool {
			runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
			return f.task(t, "t1").State == store.StateWorking
		}) {
			t.Fatalf("resumed done Worker left Task in %s", f.task(t, "t1").State)
		}
		reopened := f.task(t, "t1")
		if reopened.Launches != task.Launches || reopened.AgentName != task.AgentName || reopened.PaneID != task.PaneID || reopened.Branch != task.Branch || reopened.PRURL != task.PRURL || foregroundPID(t, f, task.PaneID) != pid {
			t.Fatalf("watcher reopening changed the Worker instance or custody: before=%#v after=%#v", task, reopened)
		}
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			t.Fatal(err)
		}
		var transitions, notices int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM transitions WHERE task_id=? AND from_state='done' AND to_state='working' AND source='watcher'`, task.ID).Scan(&transitions); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='worker_resumed'`, task.ID).Scan(&notices); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.Close()
		if transitions != 1 || notices != 1 {
			t.Fatalf("done Worker reopening recorded %d transitions and %d Notices, want one of each", transitions, notices)
		}
	}
	for _, args := range [][]string{{"teardown", "t1"}, {"teardown", "t1", "--discard"}, {"teardown", "t1", "--discard", "--user-approved", " "}} {
		command := exec.Command(f.binary, args...)
		command.Dir, command.Env = f.repo, f.leadEnv
		if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "teardown_refused") {
			t.Fatalf("unapproved cancellation was not refused: args=%v err=%v output=%s", args, err, output)
		}
		f.assertAlive(t, f.snapshot(t), task.PaneID)
	}
	workerCommand := exec.Command(f.binary, "teardown", "t1", "--discard", "--user-approved", "destroy t3 now")
	workerCommand.Dir, workerCommand.Env = task.WorktreePath, workerEnv
	if output, err := workerCommand.CombinedOutput(); err == nil || !strings.Contains(string(output), "worker_forbidden") {
		t.Fatalf("Worker cancellation was not refused: %v %s", err, output)
	}
	change := filepath.Join(task.WorktreePath, "cancel-change.txt")
	if err := os.WriteFile(change, []byte("committed cancelled work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, task.WorktreePath, "add", ".")
	gitTest(t, f.env, task.WorktreePath, "commit", "-m", "cancelled change")
	command := exec.Command(f.binary, "teardown", "t1", "--discard", "--user-approved", "destroy t3 now")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "torn-down") {
		t.Fatalf("approved cancellation did not stop and discard the working Worker: err=%v output=%s", err, output)
	}
	if current := f.task(t, "t1"); current.State != store.StateTornDown {
		t.Fatalf("approved cancellation left Task in %s", current.State)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var approvals int
	var quote, approvedTip string
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*),MAX(user_quote),MAX(branch_sha) FROM approvals WHERE task_id=? AND action='discard'`, task.ID).Scan(&approvals, &quote, &approvedTip); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	for _, sandbox := range sandboxes {
		if sandbox.ID == task.SandboxID && (sandbox.State == "held" || sandbox.TaskID != 0) {
			t.Fatalf("cancellation kept the Sandbox held: %#v", sandbox)
		}
	}
	if approvals != 1 || quote != "destroy t3 now" {
		t.Fatalf("cancellation approvals = %d, quote %q", approvals, quote)
	}
	for _, name := range []string{"worker-interrupted", "worker-terminated"} {
		if _, err := os.Stat(filepath.Join(f.root, name)); err != nil {
			t.Fatalf("Worker did not receive verified %s: %v (pid %d)", name, err, pid)
		}
	}
	if _, _, err := store.ProcessIdentityForPID(pid); err == nil {
		t.Fatalf("Worker process %d survived cancellation", pid)
	}
	f.assertGone(t, f.snapshot(t), task.PaneID)
	f.assertAlive(t, f.snapshot(t), f.leadPaneID, f.userTab.RootPane.PaneID)
	bundles, err := filepath.Glob(filepath.Join(f.home, "projects", "shop", "tasks", "t1", "discard", "*.bundle"))
	if err != nil || len(bundles) != 1 {
		t.Fatalf("cancelled commits were not captured: %v %v", bundles, err)
	}
	if contents := strings.TrimSpace(gitTest(t, f.env, f.repo, "show", approvedTip+":cancel-late.txt")); contents != "late Worker commit" {
		t.Fatalf("discard omitted the commit completed during interruption: %q", contents)
	}
	if heads := gitTest(t, f.env, f.repo, "bundle", "list-heads", bundles[0]); !strings.Contains(heads, approvedTip) {
		t.Fatalf("discard bundle did not retain the final approved tip %s: %s", approvedTip, heads)
	}
	if output, err := gitCommand(f.env, f.repo, "rev-parse", "--verify", "refs/heads/"+task.Branch); err == nil {
		t.Fatalf("cancellation kept Task branch: %s", output)
	}
}
