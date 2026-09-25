package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExternalCommandsDisableInteractivePrompts(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s|%s' \"${GIT_TERMINAL_PROMPT:-missing}\" \"${GH_PROMPT_DISABLED:-missing}\"\n"
	for _, name := range []string{"gh", "no-mistakes", "git"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GH_PROMPT_DISABLED", "0")
	for _, name := range []string{"gh", "no-mistakes", "git"} {
		output, err := commandOutputArgs(context.Background(), root, name, "status")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "0|0"
		if name != "git" {
			want = "0|1"
		}
		if output != want {
			t.Errorf("%s prompt environment = %q, want %q", name, output, want)
		}
	}
}

func TestExternalGitHubAndFetchCommandsHaveDeadlines(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gh", "git"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	previousTimeout := externalCommandTimeout
	externalCommandTimeout = 10 * time.Millisecond
	t.Cleanup(func() { externalCommandTimeout = previousTimeout })

	for _, command := range []struct {
		name string
		args []string
	}{{name: "gh", args: []string{"api", "graphql"}}, {name: "git", args: []string{"-C", root, "fetch", "origin"}}} {
		_, err := commandOutputArgs(context.Background(), root, command.name, command.args...)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s %v error = %v, want deadline exceeded", command.name, command.args, err)
		}
	}
}

func TestExternalTimeoutKillsWrapperChildrenForGHGitFetchAndGates(t *testing.T) {
	root := t.TempDir()
	originalPath := os.Getenv("PATH")
	previousTimeout := externalCommandTimeout
	externalCommandTimeout = time.Second
	t.Cleanup(func() { externalCommandTimeout = previousTimeout })

	ghBin := filepath.Join(root, "gh-bin")
	if err := os.MkdirAll(ghBin, 0o700); err != nil {
		t.Fatal(err)
	}
	ghChildPID := filepath.Join(root, "gh-child.pid")
	if err := writeHangingChildWrapper(filepath.Join(ghBin, "gh"), ghChildPID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ghBin+string(os.PathListSeparator)+originalPath)
	assertExternalCommandTimesOutAndReapsChild(t, ghChildPID, func() (string, error) {
		return commandOutputArgs(context.Background(), root, "gh", "api", "graphql")
	})

	transportBin := filepath.Join(root, "transport-bin")
	if err := os.MkdirAll(transportBin, 0o700); err != nil {
		t.Fatal(err)
	}
	sshChildPID := filepath.Join(root, "ssh-child.pid")
	sshPath := filepath.Join(transportBin, "ssh")
	if err := writeHangingChildWrapper(sshPath, sshChildPID); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	gitTest(t, repo, "remote", "add", "origin", "ssh://git@example.invalid/owner/repo.git")
	t.Setenv("PATH", transportBin+string(os.PathListSeparator)+originalPath)
	t.Setenv("GIT_SSH_COMMAND", sshPath)
	assertExternalCommandTimesOutAndReapsChild(t, sshChildPID, func() (string, error) {
		return commandOutputArgs(context.Background(), repo, "git", "fetch", "origin")
	})

	gateChildPID := filepath.Join(root, "gate-child.pid")
	gatePath := filepath.Join(root, "gate.sh")
	if err := writeHangingChildWrapper(gatePath, gateChildPID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gatePath, 0o700); err != nil {
		t.Fatal(err)
	}
	gateContext, cancel := context.WithTimeout(context.Background(), externalCommandTimeout)
	defer cancel()
	assertExternalCommandTimesOutAndReapsChild(t, gateChildPID, func() (string, error) {
		return commandOutput(gateContext, root, gatePath)
	})
}

func writeHangingChildWrapper(path, childPIDFile string) error {
	script := "#!/bin/sh\n/bin/sleep 30 &\necho $! > " + childPIDFile + "\nwait\n"
	return os.WriteFile(path, []byte(script), 0o700)
}

func assertExternalCommandTimesOutAndReapsChild(t *testing.T, childPIDFile string, run func() (string, error)) {
	t.Helper()
	started := time.Now()
	_, err := run()
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("command error = %v, want deadline exceeded", err)
	}
	if maximum := externalCommandTimeout + externalCommandWaitDelay + 750*time.Millisecond; elapsed > maximum {
		t.Fatalf("timed-out command took %s, exceeding timeout plus WaitDelay (%s)", elapsed, maximum)
	}
	pidBytes, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatalf("wrapper did not record its child PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid < 1 {
		t.Fatalf("wrapper recorded invalid child PID %q: %v", pidBytes, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		alive, stateErr := processIsRunning(pid)
		if stateErr != nil {
			t.Fatalf("inspect child process %d: %v", pid, stateErr)
		}
		if !alive {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	alive, stateErr := processIsRunning(pid)
	if stateErr != nil || alive {
		t.Fatalf("timed-out command left child process %d running (alive=%v err=%v)", pid, alive, stateErr)
	}
}

func processIsRunning(pid int) (bool, error) {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	fields := strings.Fields(string(stat)[closeParen+1:])
	if closeParen < 0 || len(fields) == 0 {
		return false, fmt.Errorf("invalid /proc stat for PID %d", pid)
	}
	return fields[0] != "Z" && fields[0] != "X", nil
}

func TestExternalTimeoutPolicyCoversNoMistakesAndOnlyGitFetch(t *testing.T) {
	for _, command := range []struct {
		name string
		args []string
		want bool
	}{{"gh", []string{"pr", "view"}, true}, {"no-mistakes", []string{"axi", "status"}, true}, {"git", []string{"-C", "/tmp/repo", "fetch", "origin"}, true}, {"git", []string{"-C", "/tmp/repo", "status"}, false}} {
		got := timeoutForExternalCommand(command.name, command.args) > 0
		if got != command.want {
			t.Errorf("timeoutForExternalCommand(%q, %v) = %v, want %v", command.name, command.args, got, command.want)
		}
	}
}
