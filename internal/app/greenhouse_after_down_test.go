package app

import (
	"context"
	"errors"
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

	"github.com/thanhbinh1905/posse/internal/greenhouse"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseAfterDownUsesStubbedSelfExec(t *testing.T) {
	home, err := os.MkdirTemp(os.Getenv("TMPDIR"), "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	greenhouseHome := greenhouse.HomeFor(home)
	if err := os.MkdirAll(greenhouseHome, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(greenhouseHome, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(filepath.Join(greenhouseHome, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(greenhouseHome, "state.json"), []byte(`{"runs":{"active":{"status":"running"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shellBinary, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(home, "waiter.pid")
	baseline, err := appTestProcessTree(t, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	previous := selfExecCommand
	calls := 0
	selfExecCommand = func(args ...string) (*exec.Cmd, error) {
		calls++
		if len(args) != 2 || args[0] != "_greenhouse" || args[1] != "stop" {
			t.Errorf("self-exec args = %q, want _greenhouse stop", args)
		}
		childArgs := append([]string{"-c", `printf '%s\n' "$$" > "$1"; shift; exec "$@"`, "sh", pidFile, testBinary}, args...)
		return exec.Command(shellBinary, childArgs...), nil
	}
	t.Cleanup(func() { selfExecCommand = previous })

	message, err := greenhouseAfterDown(context.Background(), db, home)
	if err != nil || message != "stops after its reviews settle" {
		t.Fatalf("greenhouseAfterDown = %q, %v", message, err)
	}
	if calls != 1 {
		t.Fatalf("self-exec calls = %d, want 1", calls)
	}
	pid, err := waiterProcessPID(t, pidFile)
	if err != nil {
		t.Fatal(err)
	}
	waitForReleasedProcessExit(t, pid, os.Getpid(), baseline)
	log, err := os.ReadFile(filepath.Join(greenhouseHome, "posse-daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "refuses to run CLI command") {
		t.Fatalf("waiter child did not take the TestMain guard: %s", log)
	}
}

func TestCanSignalProcessPIDRejectsInvalidPID(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if canSignalProcessPID(pid) {
			t.Fatalf("invalid PID %d was accepted", pid)
		}
	}
}

func canSignalProcessPID(pid int) bool {
	return pid > 1
}

func waiterProcessPID(t *testing.T, path string) (int, error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && canSignalProcessPID(pid) {
				return pid, nil
			}
			return 0, fmt.Errorf("invalid waiter PID %q: %v", data, parseErr)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, fmt.Errorf("waiter PID file %s was not created", path)
}

func waitForReleasedProcessExit(t *testing.T, pid, rootPID, baseline int) {
	t.Helper()
	if !canSignalProcessPID(pid) {
		t.Fatalf("refusing to signal invalid PID %d", pid)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		count, err := appTestProcessTree(t, rootPID)
		if err != nil {
			t.Fatal(err)
		}
		if count > baseline+1 {
			killAppTestDescendants(t, rootPID)
			t.Fatalf("app.test process count multiplied: baseline=%d current=%d", baseline, count)
		}
		running, err := waiterProcessIsRunning(t, pid)
		if err != nil {
			t.Fatalf("check waiter child %d: %v", pid, err)
		}
		if !running {
			assertNoAppTestProcessMultiplication(t, rootPID, baseline)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	killAppTestDescendants(t, rootPID)
	if !canSignalProcessPID(pid) {
		t.Fatalf("refusing to signal invalid PID %d", pid)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	cleanupDeadline := time.Now().Add(time.Second)
	for time.Now().Before(cleanupDeadline) {
		running, err := waiterProcessIsRunning(t, pid)
		if err == nil && !running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waiter child %d remained after its bounded lifetime", pid)
}

func waiterProcessIsRunning(t *testing.T, pid int) (bool, error) {
	t.Helper()
	if !canSignalProcessPID(pid) {
		t.Fatalf("refusing to inspect invalid PID %d", pid)
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 || len(data) <= end+2 {
		return false, fmt.Errorf("invalid /proc stat for PID %d", pid)
	}
	return data[end+2] != 'Z', nil
}

func assertNoAppTestProcessMultiplication(t *testing.T, rootPID, baseline int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		count, err := appTestProcessTree(t, rootPID)
		if err != nil {
			t.Fatal(err)
		}
		if count > baseline {
			killAppTestDescendants(t, rootPID)
			t.Fatalf("app.test process count grew from %d to %d", baseline, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
	count, err := appTestProcessTree(t, rootPID)
	if err != nil {
		t.Fatal(err)
	}
	if count != baseline {
		t.Fatalf("app.test process count after waiter exit = %d, want %d", count, baseline)
	}
}

func appTestProcessTree(t *testing.T, rootPID int) (int, error) {
	t.Helper()
	output, err := exec.Command("ps", "-eo", "pid=,ppid=,stat=,comm=").Output()
	if err != nil {
		return 0, err
	}
	type process struct {
		pid, parent int
		state, name string
	}
	var processes []process
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		pid, parseErr := strconv.Atoi(fields[0])
		if parseErr != nil {
			continue
		}
		parent, parseErr := strconv.Atoi(fields[1])
		if parseErr != nil {
			continue
		}
		processes = append(processes, process{pid: pid, parent: parent, state: fields[2], name: fields[3]})
	}
	parents := map[int]bool{rootPID: true}
	count := 0
	for changed := true; changed; {
		changed = false
		for _, process := range processes {
			if parents[process.parent] && !parents[process.pid] {
				parents[process.pid] = true
				if process.name == "app.test" && !strings.HasPrefix(process.state, "Z") {
					count++
				}
				changed = true
			}
		}
	}
	return count, nil
}

func killAppTestDescendants(t *testing.T, rootPID int) {
	t.Helper()
	output, err := exec.Command("ps", "-eo", "pid=,ppid=,comm=").Output()
	if err != nil {
		t.Fatalf("list app.test descendants for cleanup: %v", err)
	}
	children := map[int]int{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "app.test" {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr == nil && parentErr == nil {
			children[pid] = parent
		}
	}
	for pid, parent := range children {
		if !canSignalProcessPID(pid) {
			continue
		}
		for parent != 0 && parent != rootPID {
			parent = children[parent]
		}
		if parent == rootPID {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
