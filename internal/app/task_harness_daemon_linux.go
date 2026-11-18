//go:build linux

package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func stopTaskHarnessDaemons(root string) error {
	harnessPath, err := exec.LookPath("codex")
	if err != nil {
		return nil
	}
	harnessPath, err = filepath.Abs(harnessPath)
	if err != nil {
		return err
	}
	root, err = normalizeOwnedProcessRoot(root)
	if err != nil {
		return err
	}
	pids, err := processIDsInRoots([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}})
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if pid <= 1 || !knownHarnessDaemonInTaskPath(pid, root, harnessPath) {
			continue
		}
		bootID, startTime, err := store.ProcessIdentityForPID(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect Task harness daemon %d: %w", pid, err)
		}
		handle, err := openSandboxProcessHandle(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("retain Task harness daemon %d: %w", pid, err)
		}
		if handle.Identity() != bootID+"/"+startTime {
			_ = handle.Close()
			continue
		}
		stillOwned := processReferencesRoots(pid, []ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}) && knownHarnessDaemonInTaskPath(pid, root, harnessPath) && handle.Identity() == bootID+"/"+startTime
		if !stillOwned {
			_ = handle.Close()
			continue
		}
		if err := handle.Signal(syscall.SIGTERM); err != nil {
			_ = handle.Close()
			return fmt.Errorf("stop Task harness daemon %d: %w", pid, err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			alive, err := handle.Alive()
			if err != nil {
				_ = handle.Close()
				return fmt.Errorf("check Task harness daemon %d: %w", pid, err)
			}
			if !alive || !processReferencesRoots(pid, []ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		alive, err := handle.Alive()
		_ = handle.Close()
		if err != nil {
			return fmt.Errorf("check Task harness daemon %d: %w", pid, err)
		}
		if alive && processReferencesRoots(pid, []ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}) {
			return fmt.Errorf("task harness daemon %d remains in the Sandbox", pid)
		}
	}
	return nil
}

func knownHarnessDaemonInTaskPath(pid int, root, harnessPath string) bool {
	if pid <= 1 || !processReferencesRoots(pid, []ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}}) {
		return false
	}
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	commandLine, err := os.ReadFile(filepath.Join(proc, "cmdline"))
	if err != nil {
		return false
	}
	args := strings.Split(strings.TrimRight(string(commandLine), "\x00"), "\x00")
	processExecutable, executableErr := os.Stat(filepath.Join(proc, "exe"))
	harnessExecutable, harnessErr := os.Stat(harnessPath)
	if executableErr != nil || harnessErr != nil {
		return false
	}
	isHarnessExecutable := os.SameFile(processExecutable, harnessExecutable)
	isHarnessScript := false
	if contents, readErr := os.ReadFile(harnessPath); readErr == nil && strings.HasPrefix(string(contents), "#!") {
		isHarnessScript = true
	}
	for index, arg := range args {
		if filepath.Clean(arg) != filepath.Clean(harnessPath) || !isHarnessExecutable && !isHarnessScript {
			continue
		}
		if index+1 < len(args) && args[index+1] == "app-server" || index+2 < len(args) && args[index+2] == "app-server" {
			return true
		}
	}
	return false
}
