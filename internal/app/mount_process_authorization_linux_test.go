//go:build linux

package app

import (
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStopMountProcessesRejectsUnmatchedBeforeSignaling(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "mount")
	initRepo(t, repo)
	gitTest(t, repo, "worktree", "add", "--detach", worktree, "main")

	start := func() *exec.Cmd {
		t.Helper()
		command := exec.Command("sleep", "60")
		command.Dir = worktree
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = command.Process.Kill()
			_ = command.Wait()
		})
		return command
	}
	authorized := start()
	unmatched := start()
	authorization := newMountProcessAuthorization()
	if _, err := authorization.bind(authorized.Process.Pid); err != nil {
		t.Fatal(err)
	}
	defer authorization.Close()

	if _, err := stopMountProcessesAuthorized(worktree, authorization); err == nil || !strings.Contains(err.Error(), "unverified process") {
		t.Fatalf("cleanup with an unmatched Mount process = %v, want fail-closed refusal", err)
	}
	for name, command := range map[string]*exec.Cmd{"authorized": authorized, "unmatched": unmatched} {
		if err := syscall.Kill(command.Process.Pid, 0); err != nil {
			t.Fatalf("%s process %d was signaled before rejecting the unmatched process: %v", name, command.Process.Pid, err)
		}
	}
}
