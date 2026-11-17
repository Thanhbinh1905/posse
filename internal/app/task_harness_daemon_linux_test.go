//go:build linux

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func TestStopTaskHarnessDaemonInSandbox(t *testing.T) {
	root := t.TempDir()
	defer func() { _ = stopTaskHarnessDaemons(root) }()
	binary := filepath.Join(root, "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	command := exec.Command("setsid", "sh", "-c", `("$1" app-server) </dev/null >/dev/null 2>&1 & exit 0`, "sh", binary)
	command.Dir = root
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		pids, err := processIDsInRoots([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}})
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range pids {
			parent, err := sandboxProcessParentID(candidate)
			if err == nil && parent != command.Process.Pid && knownHarnessDaemonInTaskPath(candidate, root, binary) {
				pid = candidate
				break
			}
		}
		if pid > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 1 {
		t.Fatal("fake harness did not leave a re-parented app-server in the Sandbox")
	}
	if err := stopTaskHarnessDaemons(root); err != nil {
		t.Fatalf("stop detached fake harness daemon: %v", err)
	}
	if processReferencesRoots(pid, []ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}) {
		t.Fatalf("daemon PID %d still references the Sandbox", pid)
	}
}

func TestUnknownSandboxProcessIsNotSignalledAndRefusalGivesCommand(t *testing.T) {
	root := t.TempDir()
	command := exec.Command("sleep", "30")
	command.Dir = root
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if command.Process.Pid <= 1 {
		t.Fatalf("test process has invalid PID %d", command.Process.Pid)
	}
	t.Cleanup(func() {
		if command.Process.Pid > 1 {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	if err := stopTaskHarnessDaemons(root); err != nil {
		t.Fatalf("unknown process scan: %v", err)
	}
	if !processReferencesRoots(command.Process.Pid, []ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}) {
		t.Fatal("unknown process was stopped")
	}
	err := requireNoSandboxProcesses(root, true)
	if err == nil {
		t.Fatal("discard accepted a Sandbox with an unknown process")
	}
	failure, ok := err.(*axi.Error)
	if !ok {
		t.Fatalf("refusal type = %T", err)
	}
	details := failure.Message + "\n" + strings.Join(failure.Help, "\n")
	for _, want := range []string{strconv.Itoa(command.Process.Pid), "sleep 30", "kill -TERM -- " + strconv.Itoa(command.Process.Pid)} {
		if !strings.Contains(details, want) {
			t.Errorf("refusal %q does not include %q", details, want)
		}
	}
}
