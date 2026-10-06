//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/thanhbinh1905/posse/internal/app"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type teardownCloseGuardAdapter struct {
	*herdr.Client
	beforeClose func()
	closed      string
}

func (adapter *teardownCloseGuardAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "workspace.close" || method == "tab.close" || method == "pane.close" {
		adapter.closed = method
		if adapter.beforeClose != nil {
			callback := adapter.beforeClose
			adapter.beforeClose = nil
			callback()
		}
	}
	return adapter.Client.Call(ctx, method, params)
}

type teardownShellSignalRaceAdapter struct {
	*herdr.Client
	home      string
	taskID    int64
	paneID    string
	onReplace func()
	replaced  bool
}

func (adapter *teardownShellSignalRaceAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "pane.process_info" && params["pane_id"] == adapter.paneID && !adapter.replaced && adapter.mountStopInShell(adapter.Client, ctx) {
		adapter.replaced = true
		adapter.onReplace()
	}
	return adapter.Client.Call(ctx, method, params)
}

func (adapter *teardownShellSignalRaceAdapter) mountStopInShell(client *herdr.Client, ctx context.Context) bool {
	db, err := store.OpenReadOnly(adapter.home)
	if err != nil {
		return false
	}
	intent, err := db.IntentByTask(ctx, adapter.taskID)
	_ = db.Close()
	if err != nil || intent.Step != "in_progress:mount.stop" {
		return false
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return false
	}
	pane, found := snapshotPaneByID(snapshot, adapter.paneID)
	return found && pane.Agent == ""
}

func snapshotPaneByID(snapshot herdr.Snapshot, paneID string) (herdr.Pane, bool) {
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return pane, true
		}
	}
	return herdr.Pane{}, false
}

// TestTeardownNeverClosesAReplacementAtThePaneCloseBoundary makes a real
// same-kind replacement if teardown attempts any unconditional close RPC.
func TestTeardownNeverClosesAReplacementAtThePaneCloseBoundary(t *testing.T) {
	f := newRiderTabsFixture(t)
	ctx := context.Background()
	task := f.ride(t, "t1", "Close race", "close-race")
	f.fail(t, "t1")
	work := filepath.Join(task.WorktreePath, "foreign-work.txt")
	if err := os.WriteFile(work, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := &teardownCloseGuardAdapter{Client: f.client}
	ownedPID := foregroundPID(t, f, task.PaneID)
	foreignPID := 0
	adapter.beforeClose = func() {
		if _, err := f.client.Run(ctx, "agent", "send-keys", task.PaneID, "ctrl+c"); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(10*time.Second, func() bool {
			for _, pane := range f.snapshot(t).Panes {
				if pane.PaneID == task.PaneID {
					return pane.Agent == ""
				}
			}
			return false
		}) {
			t.Fatal("owned agent did not return to shell")
		}
		if _, err := f.client.Call(ctx, "pane.rename", map[string]any{"pane_id": task.PaneID, "label": "foreign-at-close"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Run(ctx, "agent", "start", "foreign-at-close", "--kind", "claude", "--pane", task.PaneID); err != nil {
			t.Fatal(err)
		}
		foreignPID = foregroundPID(t, f, task.PaneID)
	}
	for _, entry := range f.leadEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			t.Setenv(key, value)
		}
	}
	t.Chdir(f.repo)
	cli := app.New(f.home, adapter).CLI()
	var output bytes.Buffer
	cli.Out, cli.ErrOut = &output, &output
	code := cli.Run([]string{"unsaddle", "t1", "--discard", "--user-approved", "User approved this discard"})
	if adapter.closed != "" || foreignPID != 0 {
		t.Fatalf("teardown issued unsafe %s after creating replacement PID %d: %s", adapter.closed, foreignPID, output.String())
	}

	paneAlive := false
	for _, pane := range f.snapshot(t).Panes {
		if pane.PaneID == task.PaneID {
			paneAlive = true
		}
	}
	contents, fileErr := os.ReadFile(work)
	branchOut, branchErr := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch).CombinedOutput()
	taskAfter := f.task(t, "t1")
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	mounts, mountErr := db.Mounts(ctx, f.projectID)
	_ = db.Close()
	if code == 0 {
		if taskAfter.State != store.StateTornDown || paneAlive || branchErr == nil || mountErr != nil || len(mounts) != 1 || mounts[0].State != "idle" {
			t.Fatalf("successful teardown did not rely on Herdr's natural pane exit: pane=%v branchErr=%v Task=%s Mounts=%#v output=%s", paneAlive, branchErr, taskAfter.State, mounts, output.String())
		}
		return
	}
	if !strings.Contains(output.String(), "unsaddle_incomplete") || !paneAlive || fileErr != nil || string(contents) != "preserve\n" || branchErr != nil || taskAfter.State != store.StateFailed || mountErr != nil || len(mounts) != 1 || mounts[0].State != "held" {
		t.Fatalf("fail-closed teardown did not preserve the pane and work: pane=%v file=%q fileErr=%v branch=%s Task=%s Mounts=%#v output=%s", paneAlive, contents, fileErr, branchOut, taskAfter.State, mounts, output.String())
	}
	if ownedPID <= 1 {
		t.Fatalf("invalid owned process PID %d", ownedPID)
	}
}

func TestTeardownRefusesRetainedShellKillAfterForeignReplacement(t *testing.T) {
	f := newRiderTabsFixture(t)
	ctx := context.Background()
	task := f.ride(t, "t1", "Shell signal race", "shell-signal-race")
	f.fail(t, "t1")
	work := filepath.Join(task.WorktreePath, "foreign-work.txt")
	if err := os.WriteFile(work, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownedPID := foregroundPID(t, f, task.PaneID)
	foreignPID := 0
	adapter := &teardownShellSignalRaceAdapter{
		Client: f.client, home: f.home, taskID: task.ID, paneID: task.PaneID,
		onReplace: func() {
			if _, err := f.client.Call(ctx, "pane.rename", map[string]any{"pane_id": task.PaneID, "label": "foreign-at-shell-kill"}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.client.Run(ctx, "agent", "start", "foreign-at-shell-kill", "--kind", "claude", "--pane", task.PaneID); err != nil {
				t.Fatal(err)
			}
			foreignPID = foregroundPID(t, f, task.PaneID)
		},
	}
	for _, entry := range f.leadEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			t.Setenv(key, value)
		}
	}
	t.Chdir(f.repo)
	cli := app.New(f.home, adapter).CLI()
	var output bytes.Buffer
	cli.Out, cli.ErrOut = &output, &output
	code := cli.Run([]string{"unsaddle", "t1", "--discard", "--user-approved", "User approved this discard"})
	if !adapter.replaced || foreignPID <= 1 || foreignPID == ownedPID {
		t.Fatalf("replacement missed the retained-shell kill boundary: replaced=%v owned=%d foreign=%d output=%s", adapter.replaced, ownedPID, foreignPID, output.String())
	}
	foreignAlive := syscall.Kill(foreignPID, 0) == nil
	paneAlive := false
	for _, pane := range f.snapshot(t).Panes {
		if pane.PaneID == task.PaneID {
			paneAlive = true
		}
	}
	contents, fileErr := os.ReadFile(work)
	branchOut, branchErr := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch).CombinedOutput()
	taskAfter := f.task(t, "t1")
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	mounts, mountErr := db.Mounts(ctx, f.projectID)
	_ = db.Close()
	t.Logf("retained shell replacement: owned=%d foreign=%d exit=%d alive=%v pane=%v file=%q fileErr=%v branchErr=%v Task=%s Mounts=%#v output=%s", ownedPID, foreignPID, code, foreignAlive, paneAlive, contents, fileErr, branchErr, taskAfter.State, mounts, output.String())
	if code == 0 || !strings.Contains(output.String(), "unsaddle_incomplete") || !foreignAlive || !paneAlive || fileErr != nil || string(contents) != "preserve\n" || branchErr != nil || mountErr != nil || taskAfter.State != store.StateFailed || len(mounts) != 1 || mounts[0].State != "held" {
		t.Fatalf("foreign replacement was damaged by retained-shell signaling: branch=%s", branchOut)
	}
}

func TestTeardownPidfdLauncherHelper(t *testing.T) {
	if os.Getenv("POSSE_T193_BLOCK_PIDFD") != "1" {
		return
	}
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: uint32(unix.SYS_PIDFD_OPEN)},
		{Code: unix.BPF_RET | unix.BPF_K, K: uint32(unix.SECCOMP_RET_ERRNO) | uint32(unix.ENOSYS)},
		{Code: unix.BPF_RET | unix.BPF_K, K: uint32(unix.SECCOMP_RET_ALLOW)},
	}
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatalf("set no-new-privileges: %v", err)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		t.Fatalf("install pidfd seccomp filter: %v", err)
	}
	runtime.KeepAlive(filters)
	runtime.KeepAlive(program)
	binary := os.Getenv("POSSE_T193_PIDFD_EXEC")
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("POSSE_T193_PIDFD_ARGS")), &args); err != nil {
		t.Fatalf("decode CLI args: %v", err)
	}
	if err := unix.Exec(binary, append([]string{binary}, args...), os.Environ()); err != nil {
		t.Fatalf("exec Posse with pidfd disabled: %v", err)
	}
}

func TestTeardownWithoutPidfdPreservesResourcesAndReportsCapability(t *testing.T) {
	f := newRiderTabsFixture(t)
	task := f.ride(t, "t1", "No pidfd", "no-pidfd")
	f.fail(t, "t1")
	ownedPID := foregroundPID(t, f, task.PaneID)
	work := filepath.Join(task.WorktreePath, "preserved-work.txt")
	if err := os.WriteFile(work, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{"unsaddle", "t1", "--discard", "--user-approved", "User approved this discard"})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestTeardownPidfdLauncherHelper$")
	command.Dir, command.Env = f.repo, append(append([]string(nil), f.leadEnv...), "POSSE_T193_BLOCK_PIDFD=1", "POSSE_T193_PIDFD_EXEC="+f.binary, "POSSE_T193_PIDFD_ARGS="+string(args))
	output, runErr := command.CombinedOutput()
	t.Logf("no-pidfd CLI: owned=%d error=%v output=%s", ownedPID, runErr, output)
	if runErr == nil || !strings.Contains(string(output), "unsaddle_incomplete") || !strings.Contains(string(output), "pidfd is unavailable") {
		t.Fatal("real CLI did not retain the unsupported-pidfd refusal")
	}
	if err := syscall.Kill(ownedPID, 0); err != nil {
		t.Fatalf("owned agent was signaled after capability refusal: %v", err)
	}
	if contents, err := os.ReadFile(work); err != nil || string(contents) != "preserve\n" {
		t.Fatalf("work damaged: %q %v", contents, err)
	}
	if after := f.task(t, "t1"); after.State != store.StateFailed {
		t.Fatalf("Task changed: %#v", after)
	}
	if output, err := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch).CombinedOutput(); err != nil {
		t.Fatalf("branch changed: %v %s", err, output)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mounts, err := db.Mounts(context.Background(), f.projectID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "held" || mounts[0].TaskID != task.ID {
		t.Fatalf("Mount changed: %#v %v", mounts, err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	if err != nil {
		t.Fatal(err)
	}
	foundCapabilityNotice := false
	for _, notice := range notices {
		foundCapabilityNotice = foundCapabilityNotice || notice.Kind == "unsaddle_incomplete" && strings.Contains(notice.Summary, "pidfd is unavailable")
	}
	if !foundCapabilityNotice {
		t.Fatalf("unsaddle_incomplete Notice omitted the pidfd capability reason: %#v", notices)
	}
}
