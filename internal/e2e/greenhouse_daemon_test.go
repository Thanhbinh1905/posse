//go:build e2e && linux

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func startGreenhouseDaemon(t *testing.T, binary, home string, env []string) time.Time {
	t.Helper()
	command := exec.Command(binary, "daemon")
	command.Env = env
	if err := command.Start(); err != nil {
		t.Fatalf("start Greenhouse daemon from %s: %v", binary, err)
	}
	t.Cleanup(func() {
		stop := exec.Command(binary, "stop")
		stop.Env = env
		if output, err := stop.CombinedOutput(); err != nil {
			t.Errorf("stop explicitly started Greenhouse daemon: %v: %s", err, output)
		}
		if err := command.Wait(); err != nil {
			t.Errorf("wait for Greenhouse daemon: %v", err)
		}
	})
	pidFile := filepath.Join(home, "pid")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(pidFile)
		if err == nil {
			var pid int
			if json.Unmarshal(contents, &pid) == nil && pid > 1 {
				executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
				if err != nil {
					t.Fatalf("read Greenhouse daemon executable for PID %d: %v", pid, err)
				}
				want, _ := filepath.EvalSymlinks(binary)
				got, _ := filepath.EvalSymlinks(executable)
				if got != want {
					t.Fatalf("Greenhouse daemon PID %d runs %s, want %s", pid, got, want)
				}
				ps := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
				ps.Env = append(os.Environ(), "LC_ALL=C")
				started, err := ps.Output()
				if err != nil {
					t.Fatalf("read Greenhouse daemon start time: %v", err)
				}
				daemonStartedAt, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(string(started)), time.Local)
				if err != nil {
					t.Fatalf("parse Greenhouse daemon start time %q: %v", started, err)
				}
				version, err := exec.Command("go", "version", "-m", binary).Output()
				if err != nil || !strings.Contains(string(version), "b2f5610e07da") {
					t.Fatalf("Greenhouse binary does not identify merged timestamp commit: %v\n%s", err, version)
				}
				t.Logf("Greenhouse daemon PID %d executable=%s started=%s version=%s", pid, got, daemonStartedAt.Format(time.RFC3339), strings.TrimSpace(string(version)))
				return daemonStartedAt
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Greenhouse daemon did not write PID file %s", pidFile)
	return time.Time{}
}
