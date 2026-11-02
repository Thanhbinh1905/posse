//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
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

func TestSandboxCleanupWaitsForTransientProcessInVerifiedGroup(t *testing.T) {
	root := t.TempDir()
	leader := exec.Command("sleep", "60")
	leader.Dir = root
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopSandboxProcess(t, leader)

	marker := filepath.Join(root, "was-signaled")
	child := exec.Command("sh", "-c", "trap 'touch "+marker+"' TERM; sleep 0.05")
	child.Dir = root
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopSandboxProcess(t, child)
	deadline := time.Now().Add(time.Second)
	for {
		pids, err := sandboxProcessIDs(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(pids) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox processes did not start: %v", pids)
		}
		time.Sleep(5 * time.Millisecond)
	}

	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	handle, err := authorization.bind(leader.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	authorization.retainProcessGroup(leader.Process.Pid, handle)
	stopped, err := stopSandboxProcessesAuthorized(root, authorization)
	if err != nil {
		t.Fatalf("cleanup did not wait for the short-lived group member: %v", err)
	}
	if len(stopped) != 1 || stopped[0] != strconv.Itoa(leader.Process.Pid) {
		t.Fatalf("signaled processes = %v, want only retained process %d", stopped, leader.Process.Pid)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unretained process received a signal: stat marker=%v", err)
	}
	if pids, err := sandboxProcessIDs(root); err != nil || len(pids) != 0 {
		t.Fatalf("Sandbox processes remain after their natural exit: pids=%v err=%v", pids, err)
	}
}

func TestSandboxForegroundSignalAllowsVerifiedPaneClosedDuringConcurrentTeardown(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	attachPRFixtureSandbox(t, fixture)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskLaunch(ctx, fixture.task.ID, fixture.worktree, "w2", "w2:p1", "posse:shop:t1", agentName("shop", fixture.task.Seq, 1)); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}

	foreground := startSandboxProcess(t, fixture.worktree)
	pane := herdr.Pane{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, TabID: "w2:t1", Label: task.PaneLabel, CWD: fixture.worktree, Agent: "claude"}
	adapter := &unavailablePaneProcessAdapter{
		Fake:     fixture.service.Herdr.(*herdr.Fake),
		snapshot: herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude"}}},
	}
	fixture.service.Herdr = adapter
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	if _, err := authorization.bind(foreground.Process.Pid); err != nil {
		t.Fatal(err)
	}

	err = fixture.service.verifySandboxForegroundSignalOwnership(ctx, fixture.project, task, fixture.worktree, pane, foreground.Process.Pid, agentName("shop", task.Seq, 1), authorization)
	if err != nil {
		t.Fatalf("retained foreground process was refused after its verified pane closed: %v", err)
	}

	adapter.snapshot.Panes = append(adapter.snapshot.Panes, pane)
	err = fixture.service.verifySandboxForegroundSignalOwnership(ctx, fixture.project, task, fixture.worktree, pane, foreground.Process.Pid, agentName("shop", task.Seq, 1), authorization)
	if err == nil {
		t.Fatal("foreground process was authorized while Herdr still reported its pane")
	}
}

type unavailablePaneProcessAdapter struct {
	*herdr.Fake
	snapshot herdr.Snapshot
}

func (adapter *unavailablePaneProcessAdapter) Snapshot(context.Context) (herdr.Snapshot, error) {
	return adapter.snapshot, nil
}

func (adapter *unavailablePaneProcessAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "pane.process_info" {
		return nil, &herdr.Error{Code: "pane_process_unavailable", Message: "pane shell is no longer running"}
	}
	return adapter.Fake.Call(ctx, method, params)
}

func TestReportedCodexResearchAcceptsNamelessAgentWithRecordedSession(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	attachPRFixtureSandbox(t, fixture)
	ctx := context.Background()
	const sessionID = "01a1425b-3893-75f3-8310-79ae9e6f9b49"
	if err := fixture.db.UpdateTaskLaunch(ctx, fixture.task.ID, fixture.worktree, "w2", "w2:p1", "posse:shop:t1", agentName("shop", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET agent_session=?,agent_session_kind='codex' WHERE id=?`, `{"session_id":"`+sessionID+`"}`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}

	shell := startSandboxProcess(t, fixture.worktree)
	agent := startSandboxProcess(t, fixture.worktree)
	pane := herdr.Pane{
		PaneID:       task.PaneID,
		WorkspaceID:  task.HerdrWorkspaceID,
		TabID:        "w2:t1",
		Label:        task.PaneLabel,
		CWD:          fixture.worktree,
		Agent:        "codex",
		AgentSession: json.RawMessage(`{"agent":"codex","kind":"id","source":"herdr:codex","value":"` + sessionID + `"}`),
	}
	snapshot := herdr.Snapshot{
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude"},
			pane,
		},
		Agents: []herdr.Agent{{PaneID: pane.PaneID, Agent: "codex", SessionID: sessionID}},
	}
	adapter := &researchProcessChurnAdapter{
		Fake:             fixture.service.Herdr.(*herdr.Fake),
		snapshot:         snapshot,
		shellPID:         shell.Process.Pid,
		agentPID:         agent.Process.Pid,
		worktree:         fixture.worktree,
		leadCWD:          fixture.repo,
		agentProcessName: "codex",
	}
	fixture.service.Herdr = adapter
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	if err := fixture.service.verifySandboxForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, authorization); err != nil {
		t.Fatalf("same-session nameless Codex Worker was refused teardown ownership: %v", err)
	}
	if authorization.handle(agent.Process.Pid) == nil {
		t.Fatal("exact Codex process was not retained after ownership verification")
	}

	adapter.snapshot.Agents[0].SessionID = "replacement-session"
	adapter.processCalls = 0
	replacementAuthorization := newSandboxProcessAuthorization()
	err = fixture.service.verifySandboxForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, replacementAuthorization)
	replacementAuthorization.Close()
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("replacement session was not refused: %v", err)
	}
}

func TestTeardownOwnershipToleratesAgentReportLapsingBetweenSnapshots(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	attachPRFixtureSandbox(t, fixture)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskLaunch(ctx, fixture.task.ID, fixture.worktree, "w2", "w2:p1", "posse:shop:t1", agentName("shop", fixture.task.Seq, 1)); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	shell := startSandboxProcess(t, fixture.worktree)
	agent := startSandboxProcess(t, fixture.worktree)
	snapshot := herdr.Snapshot{
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude"},
			{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, TabID: "w2:t1", Label: task.PaneLabel, CWD: fixture.worktree, Agent: "claude"},
		},
		Agents: []herdr.Agent{{Name: agentName("shop", task.Seq, 1), PaneID: task.PaneID, Kind: "claude"}},
	}
	adapter := &researchProcessChurnAdapter{
		Fake:            fixture.service.Herdr.(*herdr.Fake),
		snapshot:        snapshot,
		shellPID:        shell.Process.Pid,
		agentPID:        agent.Process.Pid,
		worktree:        fixture.worktree,
		leadCWD:         fixture.repo,
		lapseAgentAfter: 2, // initial scan and the pre-verification recheck still see the agent
	}
	fixture.service.Herdr = adapter
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	if err := fixture.service.verifySandboxForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, authorization); err != nil {
		t.Fatalf("Herdr dropping its agent report during verification was refused although the pane still belongs to the Task: %v", err)
	}
	if authorization.handle(agent.Process.Pid) == nil {
		t.Fatal("exact Worker process was not retained after ownership verification")
	}

	adapter.snapshotCalls, adapter.lapseAgentAfter = 0, 0
	adapter.replaceAgentAfter = agentName("shop", task.Seq+1, 1)
	foreignAuthorization := newSandboxProcessAuthorization()
	defer foreignAuthorization.Close()
	err = fixture.service.verifySandboxForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, foreignAuthorization)
	if err == nil || !strings.Contains(err.Error(), "changed Herdr ownership") {
		t.Fatalf("pane taken over by another Task's agent during verification was not refused: %v", err)
	}
}

func TestReportedResearchAllowsTransientPaneProcessWithoutAuthorizingIt(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	sandbox := attachPRFixtureSandbox(t, fixture)
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET type='research' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskLaunch(ctx, fixture.task.ID, fixture.worktree, "w2", "w2:p1", "posse:shop:t1", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateWorking, store.StateDone, "worker", "Research finished"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateDone, store.StateReported, "cli", "Report saved"); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != store.StateReported || task.SandboxID != sandbox.ID {
		t.Fatalf("fixture is not an ordinary reported Research with a held Sandbox: task=%#v sandbox=%#v", task, sandbox)
	}

	shell := startSandboxProcess(t, fixture.worktree)
	agent := startSandboxProcess(t, fixture.worktree)
	transient := startSandboxProcess(t, fixture.worktree)
	snapshot := herdr.Snapshot{
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude"},
			{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, TabID: "w2:t1", Label: task.PaneLabel, CWD: fixture.worktree, Agent: "claude"},
		},
		Agents: []herdr.Agent{{Name: agentName("shop", task.Seq, 1), PaneID: task.PaneID, Kind: "claude"}},
	}
	adapter := &researchProcessChurnAdapter{
		Fake:         fixture.service.Herdr.(*herdr.Fake),
		snapshot:     snapshot,
		shellPID:     shell.Process.Pid,
		agentPID:     agent.Process.Pid,
		transientPID: transient.Process.Pid,
		worktree:     fixture.worktree,
		leadCWD:      fixture.repo,
	}
	fixture.service.Herdr = adapter
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	if err := fixture.service.verifySandboxForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, authorization); err != nil {
		t.Fatalf("ordinary Research ownership verification rejected a transient child: %v", err)
	}
	if authorization.handle(transient.Process.Pid) != nil {
		t.Fatal("transient child was added to the retained cleanup authorization")
	}

	adapter.secondForegroundGroup = shell.Process.Pid
	adapter.processCalls = 0
	foregroundChurnAuthorization := newSandboxProcessAuthorization()
	if err := fixture.service.verifySandboxForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, foregroundChurnAuthorization); err != nil {
		foregroundChurnAuthorization.Close()
		t.Fatalf("ordinary foreground switch to an already-retained shell was refused: %v", err)
	}
	foregroundChurnAuthorization.Close()
	adapter.secondForegroundGroup = transient.Process.Pid
	adapter.processCalls = 0
	foreignForegroundAuthorization := newSandboxProcessAuthorization()
	err = fixture.service.verifySandboxForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, foreignForegroundAuthorization)
	foreignForegroundAuthorization.Close()
	if err == nil || !strings.Contains(err.Error(), "unretained foreground process") {
		t.Fatalf("unretained foreground replacement was not refused: %v", err)
	}
	adapter.secondForegroundGroup = 0
	adapter.processCalls = 0

	if _, err := stopSandboxProcessesAuthorized(fixture.worktree, authorization); err == nil || !strings.Contains(err.Error(), "unverified process") {
		t.Fatalf("live unretained child cleanup error = %v, want fail-closed unverified-process refusal", err)
	}
	for name, command := range map[string]*exec.Cmd{"shell": shell, "agent": agent, "transient": transient} {
		if err := syscall.Kill(command.Process.Pid, 0); err != nil {
			t.Fatalf("%s was signaled before the unretained child check: %v", name, err)
		}
	}

	stopSandboxProcess(t, transient)
	stopped, err := stopSandboxProcessesAuthorized(fixture.worktree, authorization)
	if err != nil {
		t.Fatalf("cleanup after the transient child exited: %v", err)
	}
	if len(stopped) != 2 {
		t.Fatalf("stopped process labels = %v, want the two retained pane processes", stopped)
	}
	stopSandboxProcess(t, shell)
	stopSandboxProcess(t, agent)
}

type researchProcessChurnAdapter struct {
	*herdr.Fake
	snapshot              herdr.Snapshot
	shellPID              int
	agentPID              int
	transientPID          int
	secondForegroundGroup int
	worktree              string
	leadCWD               string
	agentProcessName      string
	processCalls          int
	snapshotCalls         int
	lapseAgentAfter       int
	replaceAgentAfter     string
}

func (adapter *researchProcessChurnAdapter) Snapshot(context.Context) (herdr.Snapshot, error) {
	adapter.snapshotCalls++
	if adapter.replaceAgentAfter != "" && adapter.snapshotCalls > 2 {
		replaced := adapter.snapshot
		replaced.Agents = []herdr.Agent{{Name: adapter.replaceAgentAfter, PaneID: adapter.snapshot.Panes[len(adapter.snapshot.Panes)-1].PaneID, Kind: "claude"}}
		return replaced, nil
	}
	if adapter.lapseAgentAfter == 0 || adapter.snapshotCalls <= adapter.lapseAgentAfter {
		return adapter.snapshot, nil
	}
	lapsed := adapter.snapshot
	lapsed.Agents = nil
	lapsed.Panes = append([]herdr.Pane(nil), adapter.snapshot.Panes...)
	for i := range lapsed.Panes {
		if lapsed.Panes[i].PaneID == adapter.snapshot.Panes[len(adapter.snapshot.Panes)-1].PaneID {
			lapsed.Panes[i].Agent = ""
		}
	}
	return lapsed, nil
}

func (adapter *researchProcessChurnAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method != "pane.process_info" {
		return adapter.Fake.Call(ctx, method, params)
	}
	paneID, _ := params["pane_id"].(string)
	cwd := adapter.worktree
	if paneID != "w2:p1" {
		cwd = adapter.leadCWD
	}
	processName := adapter.agentProcessName
	if processName == "" {
		processName = "claude"
	}
	processes := []map[string]any{{"pid": adapter.agentPID, "name": processName, "cwd": cwd}}
	foregroundGroup := adapter.agentPID
	if paneID == "w2:p1" {
		adapter.processCalls++
		if adapter.processCalls == 2 {
			processes = append(processes, map[string]any{"pid": adapter.transientPID, "name": "git", "cwd": adapter.worktree})
			if adapter.secondForegroundGroup > 1 {
				foregroundGroup = adapter.secondForegroundGroup
			}
		}
	}
	return json.Marshal(map[string]any{"process_info": map[string]any{
		"pane_id":                     paneID,
		"shell_pid":                   adapter.shellPID,
		"foreground_process_group_id": foregroundGroup,
		"foreground_processes":        processes,
	}})
}

func startSandboxProcess(t *testing.T, worktree string) *exec.Cmd {
	t.Helper()
	command := exec.Command("sleep", "60")
	command.Dir = worktree
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopSandboxProcess(t, command) })
	return command
}

func stopSandboxProcess(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if command == nil || command.Process == nil {
		return
	}
	if command.ProcessState != nil {
		return
	}
	_ = command.Process.Kill()
	if err := command.Wait(); err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Errorf("wait for fixture process %d: %v", command.Process.Pid, err)
		}
	}
}

type staleAgentRecordAdapter struct {
	*herdr.Fake
	snapshot   herdr.Snapshot
	shellPID   int
	foreground int
}

func (adapter *staleAgentRecordAdapter) Snapshot(context.Context) (herdr.Snapshot, error) {
	return adapter.snapshot, nil
}

func (adapter *staleAgentRecordAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method != "pane.process_info" {
		return adapter.Fake.Call(ctx, method, params)
	}
	paneID, _ := params["pane_id"].(string)
	return json.Marshal(map[string]any{"process_info": map[string]any{
		"pane_id":                     paneID,
		"shell_pid":                   adapter.shellPID,
		"foreground_process_group_id": adapter.foreground,
		"foreground_processes":        []map[string]any{{"pid": adapter.foreground, "name": "zsh", "cwd": ""}},
	}})
}

func TestTeardownIgnoresStaleHerdrAgentRecordOnceForegroundIsTheShell(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateWorking)
	attachPRFixtureSandbox(t, fixture)
	ctx := context.Background()
	if err := fixture.db.UpdateTaskLaunch(ctx, fixture.task.ID, fixture.worktree, "w2", "w2:p1", "posse:shop:t1", agentName("shop", fixture.task.Seq, 1)); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	shell := startSandboxProcess(t, fixture.worktree)
	pane := herdr.Pane{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, TabID: "w2:t1", Label: task.PaneLabel, CWD: fixture.worktree, Agent: "claude"}
	adapter := &staleAgentRecordAdapter{
		Fake:       fixture.service.Herdr.(*herdr.Fake),
		snapshot:   herdr.Snapshot{Panes: []herdr.Pane{pane}, Agents: []herdr.Agent{{Name: agentName("shop", task.Seq, 1), PaneID: task.PaneID, Kind: "claude"}}},
		shellPID:   shell.Process.Pid,
		foreground: shell.Process.Pid,
	}
	fixture.service.Herdr = adapter
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	if _, err := authorization.bind(shell.Process.Pid); err != nil {
		t.Fatal(err)
	}

	err = fixture.service.verifyTaskPaneShellAfterForegroundExit(ctx, fixture.project, task, fixture.worktree, pane, shell.Process.Pid, shell.Process.Pid+1, authorization)
	if err != nil && strings.Contains(err.Error(), "still exposes a foreground agent") {
		t.Fatalf("stale Herdr agent record refused teardown although the shell is the foreground: %v", err)
	}

	adapter.foreground = shell.Process.Pid + 1
	if err := fixture.service.verifyTaskPaneShellAfterForegroundExit(ctx, fixture.project, task, fixture.worktree, pane, shell.Process.Pid, shell.Process.Pid+1, authorization); err == nil {
		t.Fatal("teardown was authorized while a non-shell foreground process owns the pane")
	}
}
