package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func guardGitProcesses(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	t.Cleanup(func() {
		for _, process := range repositoryGitProcesses(root) {
			t.Errorf("git process outlived repository %s: %s", root, process)
		}
	})
}

func TestRepositoryGitProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process inspection requires /proc")
	}
	root := t.TempDir()
	other := t.TempDir()
	guardGitProcesses(t, root)
	initRepo(t, root)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = writer.Close() })
	command := exec.Command("git", "-C", root, "hash-object", "--stdin")
	command.Stdin = input
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		if err := command.Wait(); err != nil {
			t.Errorf("wait for git: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processes := repositoryGitProcesses(root); len(processes) == 1 {
			if !strings.HasPrefix(processes[0], strconv.Itoa(command.Process.Pid)+" ") {
				t.Fatalf("unexpected git process: %v", processes)
			}
			if processes := repositoryGitProcesses(other); len(processes) != 0 {
				t.Fatalf("unrelated repository matched: %v", processes)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("live git process was not detected")
}

func repositoryGitProcesses(root string) []string {
	root, _ = filepath.Abs(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	entries, _ := os.ReadDir("/proc")
	var found []string
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err != nil || pid < 2 {
			continue
		}
		proc := filepath.Join("/proc", entry.Name())
		comm, _ := os.ReadFile(filepath.Join(proc, "comm"))
		if !strings.HasPrefix(strings.TrimSpace(string(comm)), "git") {
			continue
		}
		args, _ := os.ReadFile(filepath.Join(proc, "cmdline"))
		cwd, _ := os.Readlink(filepath.Join(proc, "cwd"))
		paths := append(strings.Split(string(args), "\x00"), cwd)
		for _, path := range paths {
			if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
				found = append(found, entry.Name()+" "+strings.ReplaceAll(string(args), "\x00", " "))
				break
			}
		}
	}
	return found
}
