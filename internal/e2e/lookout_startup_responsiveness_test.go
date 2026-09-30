//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLookoutStartupGraceHonorsTimeoutAndSIGTERM(t *testing.T) {
	f := newPRLifecycleFixtureWithSlowLoginShell(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatal("initial Lookout did not start")
	}
	stuckShell := filepath.Join(f.root, "bin", "slow-login-shell")
	if err := os.WriteFile(stuckShell, []byte("#!/bin/sh\nsleep 300\nexec /bin/sh -l\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, pid := range lookoutPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatal("Lookout did not stop")
	}

	for _, mode := range []string{"timeout", "SIGTERM"} {
		timeout := "500"
		if mode == "SIGTERM" {
			timeout = "30000"
		}
		cmd := exec.Command(f.binary, "lookout", "--ack", "all", "--timeout", timeout)
		cmd.Dir, cmd.Env = f.repo, f.leadEnv
		start := time.Now()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if mode == "SIGTERM" {
			time.Sleep(500 * time.Millisecond)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		err := cmd.Wait()
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("lookout %s: %v", mode, err)
		}
		if elapsed > 2*time.Second {
			t.Errorf("lookout %s took %s during startup grace", mode, elapsed)
		}
	}
}
