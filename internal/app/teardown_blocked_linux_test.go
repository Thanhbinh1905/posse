//go:build linux

package app

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestResidualParentProcessesDropsChildrenAndExited(t *testing.T) {
	parent := exec.Command("/bin/sh", "-c", "sleep 30 & wait")
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Process.Kill(); _, _ = parent.Process.Wait() })
	var child int
	deadline := time.Now().Add(5 * time.Second)
	for child == 0 && time.Now().Before(deadline) {
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			var pid int
			if _, err := fmt.Sscan(entry.Name(), &pid); err != nil || pid <= 1 {
				continue
			}
			if ppid, err := sandboxProcessParentID(pid); err == nil && ppid == parent.Process.Pid {
				child = pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("shell child did not start")
	}
	got := residualParentProcesses([]taskProcessDetail{{PID: parent.Process.Pid}, {PID: child}, {PID: 1 << 30}})
	if len(got) != 1 || got[0].PID != parent.Process.Pid {
		t.Fatalf("residual parents = %#v, want only PID %d", got, parent.Process.Pid)
	}
}

func TestSandboxProcessStartedBefore(t *testing.T) {
	pid := os.Getpid()
	if !sandboxProcessStartedBefore(pid, time.Now().Add(time.Hour)) {
		t.Fatal("running test process should predate a future instant")
	}
	if sandboxProcessStartedBefore(pid, time.Now().Add(-24*time.Hour)) {
		t.Fatal("running test process should not predate yesterday")
	}
	if sandboxProcessStartedBefore(1<<30, time.Now()) {
		t.Fatal("missing process must be treated as new")
	}
}
