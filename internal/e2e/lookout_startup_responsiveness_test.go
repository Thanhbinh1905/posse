//go:build e2e

package e2e

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLookoutWaitHonorsTimeoutAndSIGTERMWithoutVisibleShell(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Project poller did not start: %v", lookoutPIDs(f.root))
	}
	stopLiveLookoutPoller(t, f.root)

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
		if err := cmd.Wait(); err != nil {
			t.Fatalf("lookout %s: %v", mode, err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("lookout %s took %s", mode, elapsed)
		}
	}
	if pids := lookoutPIDs(f.root); len(pids) != 1 {
		t.Fatalf("Lookout wait left %d hidden pollers: %v", len(pids), pids)
	}
}
