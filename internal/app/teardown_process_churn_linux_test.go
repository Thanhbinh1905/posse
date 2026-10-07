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

func TestMountCleanupWaitsForTransientProcessInVerifiedGroup(t *testing.T) {
	root := t.TempDir()
	leader := exec.Command("sleep", "60")
	leader.Dir = root
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopMountProcess(t, leader)

	marker := filepath.Join(root, "was-signaled")
	child := exec.Command("sh", "-c", "trap 'touch "+marker+"' TERM; sleep 0.05")
	child.Dir = root
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopMountProcess(t, child)
	deadline := time.Now().Add(time.Second)
	for {
		pids, err := mountProcessIDs(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(pids) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Mount processes did not start: %v", pids)
		}
		time.Sleep(5 * time.Millisecond)
	}

	authorization := newMountProcessAuthorization()
	defer authorization.Close()
	handle, err := authorization.bind(leader.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	authorization.retainProcessGroup(leader.Process.Pid, handle)
	stopped, err := stopMountProcessesAuthorized(root, authorization)
	if err != nil {
		t.Fatalf("cleanup did not wait for the short-lived group member: %v", err)
	}
	if len(stopped) != 1 || stopped[0] != strconv.Itoa(leader.Process.Pid) {
		t.Fatalf("signaled processes = %v, want only retained process %d", stopped, leader.Process.Pid)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unretained process received a signal: stat marker=%v", err)
	}
	if pids, err := mountProcessIDs(root); err != nil || len(pids) != 0 {
		t.Fatalf("Mount processes remain after their natural exit: pids=%v err=%v", pids, err)
	}
}

func TestReportedScoutAllowsTransientPaneProcessWithoutAuthorizingIt(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	mount := attachPRFixtureMount(t, fixture)
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET type='scout' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.UpdateTaskLaunch(ctx, fixture.task.ID, fixture.worktree, "w2", "w2:p1", "posse:shop:t1", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateWorking, store.StateDone, "worker", "Scout finished"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, fixture.task.ID, store.StateDone, store.StateReported, "cli", "Report saved"); err != nil {
		t.Fatal(err)
	}
	task, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != store.StateReported || task.MountID != mount.ID {
		t.Fatalf("fixture is not an ordinary reported Scout with a held Mount: task=%#v mount=%#v", task, mount)
	}

	shell := startMountProcess(t, fixture.worktree)
	agent := startMountProcess(t, fixture.worktree)
	transient := startMountProcess(t, fixture.worktree)
	snapshot := herdr.Snapshot{
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude"},
			{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, TabID: "w2:t1", Label: task.PaneLabel, CWD: fixture.worktree, Agent: "claude"},
		},
		Agents: []herdr.Agent{{Name: agentName("shop", task.Seq, 1), PaneID: task.PaneID, Kind: "claude"}},
	}
	adapter := &scoutProcessChurnAdapter{
		Fake:         fixture.service.Herdr.(*herdr.Fake),
		snapshot:     snapshot,
		shellPID:     shell.Process.Pid,
		agentPID:     agent.Process.Pid,
		transientPID: transient.Process.Pid,
		worktree:     fixture.worktree,
		leadCWD:      fixture.repo,
	}
	fixture.service.Herdr = adapter
	authorization := newMountProcessAuthorization()
	defer authorization.Close()
	if err := fixture.service.verifyMountForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, authorization); err != nil {
		t.Fatalf("ordinary Scout ownership verification rejected a transient child: %v", err)
	}
	if authorization.handle(transient.Process.Pid) != nil {
		t.Fatal("transient child was added to the retained cleanup authorization")
	}

	adapter.secondForegroundGroup = shell.Process.Pid
	adapter.processCalls = 0
	foregroundChurnAuthorization := newMountProcessAuthorization()
	if err := fixture.service.verifyMountForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, foregroundChurnAuthorization); err != nil {
		foregroundChurnAuthorization.Close()
		t.Fatalf("ordinary foreground switch to an already-retained shell was refused: %v", err)
	}
	foregroundChurnAuthorization.Close()
	adapter.secondForegroundGroup = transient.Process.Pid
	adapter.processCalls = 0
	foreignForegroundAuthorization := newMountProcessAuthorization()
	err = fixture.service.verifyMountForegroundOwnershipWithAuthorization(ctx, fixture.db, fixture.project, task, foreignForegroundAuthorization)
	foreignForegroundAuthorization.Close()
	if err == nil || !strings.Contains(err.Error(), "unretained foreground process") {
		t.Fatalf("unretained foreground replacement was not refused: %v", err)
	}
	adapter.secondForegroundGroup = 0
	adapter.processCalls = 0

	if _, err := stopMountProcessesAuthorized(fixture.worktree, authorization); err == nil || !strings.Contains(err.Error(), "unverified process") {
		t.Fatalf("live unretained child cleanup error = %v, want fail-closed unverified-process refusal", err)
	}
	for name, command := range map[string]*exec.Cmd{"shell": shell, "agent": agent, "transient": transient} {
		if err := syscall.Kill(command.Process.Pid, 0); err != nil {
			t.Fatalf("%s was signaled before the unretained child check: %v", name, err)
		}
	}

	stopMountProcess(t, transient)
	stopped, err := stopMountProcessesAuthorized(fixture.worktree, authorization)
	if err != nil {
		t.Fatalf("cleanup after the transient child exited: %v", err)
	}
	if len(stopped) != 2 {
		t.Fatalf("stopped process labels = %v, want the two retained pane processes", stopped)
	}
	stopMountProcess(t, shell)
	stopMountProcess(t, agent)
}

type scoutProcessChurnAdapter struct {
	*herdr.Fake
	snapshot              herdr.Snapshot
	shellPID              int
	agentPID              int
	transientPID          int
	secondForegroundGroup int
	worktree              string
	leadCWD               string
	processCalls          int
}

func (adapter *scoutProcessChurnAdapter) Snapshot(context.Context) (herdr.Snapshot, error) {
	return adapter.snapshot, nil
}

func (adapter *scoutProcessChurnAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method != "pane.process_info" {
		return adapter.Fake.Call(ctx, method, params)
	}
	paneID, _ := params["pane_id"].(string)
	cwd := adapter.worktree
	if paneID != "w2:p1" {
		cwd = adapter.leadCWD
	}
	processes := []map[string]any{{"pid": adapter.agentPID, "name": "claude", "cwd": cwd}}
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

func startMountProcess(t *testing.T, worktree string) *exec.Cmd {
	t.Helper()
	command := exec.Command("sleep", "60")
	command.Dir = worktree
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopMountProcess(t, command) })
	return command
}

func stopMountProcess(t *testing.T, command *exec.Cmd) {
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
