//go:build e2e && linux

package e2e

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

const (
	lockOwnerChildEnv = "POSSE_E2E_LOCK_OWNER_CHILD"
	lockOwnerPathEnv  = "POSSE_E2E_LOCK_OWNER_PATH"
	lockOwnerPIDEnv   = "POSSE_E2E_LOCK_OWNER_PID_FILE"
)

func TestE2ELockOwnerCannotOutliveTestProcess(t *testing.T) {
	if os.Getenv(lockOwnerChildEnv) == "1" {
		lockOwnerTestChild(t)
		return
	}

	root := t.TempDir()
	lockPath := filepath.Join(root, "owner.lock")
	pidPath := filepath.Join(root, "owner.pid")
	env := os.Environ()
	env = setEnv(env, lockOwnerChildEnv, "1")
	env = setEnv(env, lockOwnerPathEnv, lockPath)
	env = setEnv(env, lockOwnerPIDEnv, pidPath)
	child := exec.Command(os.Args[0], "-test.run=^TestE2ELockOwnerCannotOutliveTestProcess$")
	child.Env = env
	output, err := child.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("parent test process did not exit from SIGKILL: err=%v output=%s", err, output)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("parent test process exit = %v, want SIGKILL: output=%s", exitErr, output)
	}

	contents, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read lock-owner identity after parent death: %v", err)
	}
	fields := strings.Fields(string(contents))
	if len(fields) != 3 {
		t.Fatalf("lock-owner identity = %q", contents)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid < 2 {
		t.Fatalf("lock-owner PID = %q: %v", fields[0], err)
	}
	owner := teardownFixtureProcess{PID: pid, BootID: fields[1], StartTime: fields[2]}
	t.Cleanup(func() {
		if teardownFixtureProcessRunning(owner) {
			_ = syscall.Kill(owner.PID, syscall.SIGKILL)
		}
	})
	if !waitForCondition(2*time.Second, func() bool { return !teardownFixtureProcessRunning(owner) }) {
		stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		commandLine, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		t.Fatalf("lock owner %d survived its test process: stat=%q cmdline=%q", pid, stat, commandLine)
	}
	if err := exec.Command("flock", "-n", lockPath, "true").Run(); err != nil {
		t.Fatalf("lock owner %d retained its lock after parent death: %v", pid, err)
	}
}

func lockOwnerTestChild(t *testing.T) {
	owner := newE2ELockOwner("parent-death-lock-owner", os.Getenv(lockOwnerPathEnv), os.Environ())
	ready, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	if !bufio.NewScanner(ready).Scan() {
		t.Fatal("lock owner did not acquire its lock")
	}
	bootID, startTime, err := store.ProcessIdentityForPID(owner.Process.Pid)
	if err != nil {
		t.Fatalf("read lock-owner identity: %v", err)
	}
	identity := fmt.Sprintf("%d\t%s\t%s\n", owner.Process.Pid, bootID, startTime)
	if err := os.WriteFile(os.Getenv(lockOwnerPIDEnv), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatalf("kill test parent: %v", err)
	}
	t.Fatal("test parent survived SIGKILL")
}
