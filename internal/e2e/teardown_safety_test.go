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
	closed    string
	afterInfo func()
}

func (adapter *teardownCloseGuardAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "workspace.close" || method == "tab.close" || method == "pane.close" {
		adapter.closed = method
	}
	raw, err := adapter.Client.Call(ctx, method, params)
	if err == nil && method == "pane.process_info" && adapter.afterInfo != nil {
		adapter.afterInfo()
	}
	return raw, err
}

func markTaskReported(t *testing.T, f *riderTabsFixture, task store.Task) {
	t.Helper()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "E2E marks the completed Rider done"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, task.ID, store.StateDone, store.StateReported, "cli", "E2E records the completed Rider report"); err != nil {
		t.Fatal(err)
	}
}

// TestTeardownNeverClosesAReplacementAtThePaneCloseBoundary verifies that a
// replacement launched between Rider shutdown and the close guard survives.
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
	replaced := false
	replacementPID := 0
	closeReads := 0
	adapter.afterInfo = func() {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := db.IntentByTask(ctx, task.ID)
		_ = db.Close()
		if replaced || err != nil || intent.Step != "in_progress:panes.close" {
			return
		}
		closeReads++
		if closeReads != 3 {
			return
		}
		replaced = true
		if _, err := f.client.Call(ctx, "pane.clear_agent_authority", map[string]any{"pane_id": task.PaneID}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Run(ctx, "agent", "start", "close-replacement", "--kind", "claude", "--pane", task.PaneID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Run(ctx, "agent", "rename", task.PaneID, task.AgentName); err != nil {
			t.Fatal(err)
		}
		replacementPID = foregroundPID(t, f, task.PaneID)
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
	if !replaced {
		t.Fatalf("replacement did not reach the pane-close boundary: code=%d output=%s", code, output.String())
	}
	if adapter.closed != "" {
		t.Fatalf("Teardown issued an unconditional %s RPC: %s", adapter.closed, output.String())
	}

	paneAlive := false
	for _, pane := range f.snapshot(t).Panes {
		if pane.PaneID == task.PaneID {
			paneAlive = true
		}
	}
	if _, _, err := store.ProcessIdentityForPID(replacementPID); err != nil || foregroundPID(t, f, task.PaneID) != replacementPID {
		t.Fatalf("same-named close-boundary replacement was stopped: pid=%d err=%v", replacementPID, err)
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
	if code == 0 || !strings.Contains(output.String(), "unsaddle_incomplete") || !strings.Contains(output.String(), "Replacement foreground process") || !paneAlive || fileErr != nil || string(contents) != "preserve\n" || branchErr != nil || taskAfter.State != store.StateFailed || mountErr != nil || len(mounts) != 1 || mounts[0].State != "held" {
		t.Fatalf("discard did not preserve the open pane and Mount: pane=%v file=%q fileErr=%v branch=%s Task=%s Mounts=%#v output=%s", paneAlive, contents, fileErr, branchOut, taskAfter.State, mounts, output.String())
	}
}

func TestTeardownStopsRetainedShellByVerifiedIdentity(t *testing.T) {
	f := newRiderTabsFixture(t)
	task := f.ride(t, "t1", "Retained Rider shell", "retained-rider-shell")
	markTaskReported(t, f, task)
	f.exitRider(t, "t1")
	shellPID := foregroundPID(t, f, task.PaneID)
	if shellPID <= 1 {
		t.Fatalf("invalid retained shell PID %d", shellPID)
	}
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "unsaddle", "t1")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("normal Teardown did not stop the verified Rider shell: %s", output)
	}
	if !waitForCondition(5*time.Second, func() bool { return syscall.Kill(shellPID, 0) != nil }) {
		t.Fatalf("retained Rider shell PID %d survived Teardown", shellPID)
	}
	if taskAfter := f.task(t, "t1"); taskAfter.State != store.StateTornDown {
		t.Fatalf("Task state after Teardown = %s", taskAfter.State)
	}
	paneAlive := false
	for _, pane := range f.snapshot(t).Panes {
		paneAlive = paneAlive || pane.PaneID == task.PaneID
	}
	if paneAlive {
		t.Fatalf("Rider pane %s survived Teardown", task.PaneID)
	}
	if output, err := exec.Command("git", "-C", f.repo, "show-ref", "--verify", "refs/heads/"+task.Branch).CombinedOutput(); err == nil {
		t.Fatalf("finished branch survived Teardown: %s", output)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := db.Mounts(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil || len(mounts) != 1 || mounts[0].State != "idle" || mounts[0].TaskID != 0 {
		t.Fatalf("Mount was not released after verified shell cleanup: %#v %v", mounts, err)
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
	markTaskReported(t, f, task)
	ownedPID := foregroundPID(t, f, task.PaneID)
	work := filepath.Join(task.WorktreePath, "preserved-work.txt")
	if err := os.WriteFile(work, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal([]string{"unsaddle", "t1"})
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
	if after := f.task(t, "t1"); after.State != store.StateReported {
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
