//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
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
		stderr, err := cmd.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderrOutput bytes.Buffer
		cmd.Stdout = &stdout
		start := time.Now()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			scanner := bufio.NewScanner(stderr)
			readySignaled := false
			for scanner.Scan() {
				line := scanner.Text()
				stderrOutput.WriteString(line + "\n")
				if !readySignaled && strings.Contains(line, "Lookout acknowledgement committed:") {
					close(ready)
					readySignaled = true
				}
			}
		}()
		if mode == "SIGTERM" {
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Signal(syscall.SIGTERM)
				<-readDone
				_ = cmd.Wait()
				t.Fatalf("lookout did not acknowledge before waiting; stdout=%s stderr=%s", stdout.String(), stderrOutput.String())
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				<-readDone
				_ = cmd.Wait()
				t.Fatalf("signal lookout after acknowledgement: %v; stdout=%s stderr=%s", err, stdout.String(), stderrOutput.String())
			}
		}
		<-readDone
		err = cmd.Wait()
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("lookout %s: %v; stdout=%s stderr=%s", mode, err, stdout.String(), stderrOutput.String())
		}
		if elapsed > 2*time.Second {
			t.Errorf("lookout %s took %s", mode, elapsed)
		}
	}
	if pids := lookoutPIDs(f.root); len(pids) != 1 {
		t.Fatalf("Lookout wait left %d hidden pollers: %v", len(pids), pids)
	}
}
