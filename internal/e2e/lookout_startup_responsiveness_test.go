//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			for scanner.Scan() {
				line := scanner.Text()
				stderrOutput.WriteString(line + "\n")
				if strings.Contains(line, "Lookout acknowledgement committed:") {
					close(ready)
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
			t.Errorf("lookout %s took %s during startup grace", mode, elapsed)
		}
	}
}
