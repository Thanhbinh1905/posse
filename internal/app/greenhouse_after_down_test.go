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
	pgidFile := filepath.Join(home, "waiter.pgid")
	previous := selfExecCommand
	calls := 0
	selfExecCommand = func(args ...string) (*exec.Cmd, error) {
		calls++
		if len(args) != 2 || args[0] != "_greenhouse" || args[1] != "stop" {
			t.Errorf("self-exec args = %q, want _greenhouse stop", args)
		}
		childArgs := append([]string{"-c", `: > "$1"; sleep 0.05; printf '%s\n' "$$" > "$1"; shift; exec "$@"`, "sh", pgidFile, testBinary}, args...)
		waiter := exec.Command(shellBinary, childArgs...)
		waiter.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		return waiter, nil
	}
	t.Cleanup(func() { selfExecCommand = previous })

	message, err := greenhouseAfterDown(context.Background(), db, home)
	if err != nil || message != "stops after its reviews settle" {
		t.Fatalf("greenhouseAfterDown = %q, %v", message, err)
	}
	if calls != 1 {
		t.Fatalf("self-exec calls = %d, want 1", calls)
	}
	pgid, err := waiterProcessGroupID(t, pgidFile)
	if err != nil {
		t.Fatal(err)
	}
	waitForProcessGroupExit(t, pgid)
	log, err := os.ReadFile(filepath.Join(greenhouseHome, "posse-daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "refuses to run CLI command") {
		t.Fatalf("waiter child did not take the TestMain guard: %s", log)
	}
}

func TestProcessGroupIsEmptyRejectsInvalidID(t *testing.T) {
	for _, pgid := range []int{-1, 0, 1} {
		if _, err := processGroupIsEmpty(pgid); err == nil {
			t.Fatalf("invalid process group ID %d was accepted", pgid)
		}
	}
}

func canSignalProcessGroup(pgid int) bool {
	return pgid > 1
}

func waiterProcessGroupID(t *testing.T, path string) (int, error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			value := strings.TrimSpace(string(data))
			if value == "" {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			pgid, parseErr := strconv.Atoi(value)
			if parseErr == nil && canSignalProcessGroup(pgid) {
				return pgid, nil
			}
			return 0, fmt.Errorf("invalid waiter process group ID %q: %v", data, parseErr)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, fmt.Errorf("waiter process group ID file %s was not created", path)
}

func waitForProcessGroupExit(t *testing.T, pgid int) {
	t.Helper()
	if !canSignalProcessGroup(pgid) {
		t.Fatalf("refusing to signal invalid process group ID %d", pgid)
	}
	deadline := time.Now().Add(3 * time.Second)
	reaped := false
	for time.Now().Before(deadline) {
		if !reaped {
			var status syscall.WaitStatus
			waited, err := syscall.Wait4(pgid, &status, syscall.WNOHANG, nil)
			if err != nil {
				t.Fatalf("reap waiter child %d: %v", pgid, err)
			}
			reaped = waited == pgid
		}
		empty, err := processGroupIsEmpty(pgid)
		if err != nil {
			t.Fatalf("check waiter process group %d: %v", pgid, err)
		}
		if reaped && empty {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	killProcessGroup(t, pgid)
	cleanupDeadline := time.Now().Add(time.Second)
	for time.Now().Before(cleanupDeadline) {
		if !reaped {
			var status syscall.WaitStatus
			waited, err := syscall.Wait4(pgid, &status, syscall.WNOHANG, nil)
			if err != nil {
				t.Fatalf("reap waiter child %d after cleanup: %v", pgid, err)
			}
			reaped = waited == pgid
		}
		empty, err := processGroupIsEmpty(pgid)
		if err != nil {
			t.Fatalf("check waiter process group %d after cleanup: %v", pgid, err)
		}
		if reaped && empty {
			t.Fatalf("waiter process group %d exceeded its bounded lifetime", pgid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waiter process group %d remained after cleanup", pgid)
}

func processGroupIsEmpty(pgid int) (bool, error) {
	if !canSignalProcessGroup(pgid) {
		return false, fmt.Errorf("refusing to inspect invalid process group ID %d", pgid)
	}
	err := syscall.Kill(-pgid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func killProcessGroup(t *testing.T, pgid int) {
	t.Helper()
	if !canSignalProcessGroup(pgid) {
		t.Fatalf("refusing to kill invalid process group ID %d", pgid)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("kill waiter process group %d: %v", pgid, err)
	}
}
