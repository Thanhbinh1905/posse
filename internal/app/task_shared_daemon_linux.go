//go:build linux

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

func taskSharedProcessExclusions(task store.Task) ([]taskProcessDetail, map[int]bool, error) {
	pids, err := detachedSharedTaskProcessPIDs(task)
	if err != nil {
		return nil, nil, err
	}
	return processDetails(pids), taskProcessPIDSet(pids), nil
}

func detachedSharedTaskProcessPIDs(task store.Task) ([]int, error) {
	owned := map[int]bool{}
	if task.ContainmentMode == "cgroup" && task.ContainmentPath != "" {
		pids, err := cgroupProcesses(task.ContainmentPath)
		if err != nil {
			return nil, err
		}
		for _, pid := range pids {
			owned[pid] = true
		}
	}
	if task.ContainmentMarker != "" {
		pids, err := markedTaskPIDs(task.ContainmentMarker)
		if err != nil {
			return nil, err
		}
		for _, pid := range pids {
			owned[pid] = true
		}
	}

	preserved := map[int]bool{}
	for pid := range owned {
		shared, err := knownSharedTaskDaemon(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect possible shared Task daemon %d: %w", pid, err)
		}
		if !shared {
			continue
		}
		hasTaskAncestor, err := processHasTaskAncestor(pid, task)
		if err != nil {
			return nil, fmt.Errorf("verify shared Task daemon %d ancestry: %w", pid, err)
		}
		if !hasTaskAncestor {
			preserved[pid] = true
		}
	}

	// Preserve descendants of a detached shared daemon as a group. Their
	// parent may be a daemon process rather than a Worker process.
	changed := true
	for changed {
		changed = false
		for pid := range owned {
			if preserved[pid] {
				continue
			}
			parent, err := sandboxProcessParentID(pid)
			if processGone(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("inspect descendant %d of a shared Task daemon: %w", pid, err)
			}
			if preserved[parent] {
				preserved[pid] = true
				changed = true
			}
		}
	}

	pids := make([]int, 0, len(preserved))
	for pid := range preserved {
		pids = append(pids, pid)
	}
	return pids, nil
}

func processHasTaskAncestor(pid int, task store.Task) (bool, error) {
	current := pid
	seen := map[int]bool{pid: true}
	for range 1024 {
		parent, err := sandboxProcessParentID(current)
		if processGone(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if parent <= 1 || parent == current {
			return false, nil
		}
		if seen[parent] {
			return false, fmt.Errorf("process ancestry loop at PID %d", parent)
		}
		seen[parent] = true
		if task.ContainmentMode == "cgroup" && task.ContainmentPath != "" && processInCgroup(parent, task.ContainmentPath) {
			return true, nil
		}
		if task.ContainmentMarker != "" && processHasTaskMarker(parent, task.ContainmentMarker) {
			return true, nil
		}
		current = parent
	}
	return false, fmt.Errorf("process ancestry for PID %d exceeded 1024 parents", pid)
}

func knownSharedTaskDaemon(pid int) (bool, error) {
	commandLine, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "cmdline"))
	if err != nil {
		return false, err
	}
	comm, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "comm"))
	if err != nil {
		return false, err
	}
	args := strings.Fields(strings.Join(strings.Split(strings.TrimRight(string(commandLine), "\x00"), "\x00"), " "))
	return isSharedTaskDaemonCommand(strings.TrimSpace(string(comm)), args), nil
}

func detachedSharedDaemonCWDOnlyPIDs(path string, taskShared map[int]bool) (map[int]bool, error) {
	root, err := normalizeOwnedProcessRoot(path)
	if err != nil {
		return nil, err
	}
	pids, err := sandboxProcessIDs(root)
	if err != nil {
		return nil, err
	}
	preserved := make(map[int]bool)
	for _, pid := range pids {
		if !taskShared[pid] {
			continue
		}
		bootID, startTime, err := store.ProcessIdentityForPID(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("retain process identity for Sandbox PID %d: %w", pid, err)
		}
		shared, err := knownSharedSSHMasterDaemon(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect Sandbox PID %d: %w", pid, err)
		}
		if !shared {
			continue
		}
		session, err := sandboxProcessSessionID(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect session for shared Sandbox daemon %d: %w", pid, err)
		}
		if session != pid {
			continue
		}
		onlyCWD, err := processReferencesSandboxOnlyByCWD(pid, root)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect Sandbox references for shared daemon %d: %w", pid, err)
		}
		if !onlyCWD {
			continue
		}
		currentBootID, currentStartTime, err := store.ProcessIdentityForPID(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("recheck shared Sandbox daemon %d: %w", pid, err)
		}
		if bootID != currentBootID || startTime != currentStartTime {
			return nil, fmt.Errorf("shared Sandbox daemon PID %d changed identity during ownership proof", pid)
		}
		preserved[pid] = true
	}
	return preserved, nil
}

func knownSharedSSHMasterDaemon(pid int) (bool, error) {
	commandLine, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "cmdline"))
	if err != nil {
		return false, err
	}
	comm, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "comm"))
	if err != nil {
		return false, err
	}
	args := strings.Fields(strings.Join(strings.Split(strings.TrimRight(string(commandLine), "\x00"), "\x00"), " "))
	command := strings.ToLower(strings.Join(args, " "))
	commandName := ""
	if len(args) > 0 {
		commandName = strings.ToLower(filepath.Base(args[0]))
	}
	commName := strings.ToLower(strings.TrimSpace(string(comm)))
	if strings.HasPrefix(commName, "ssh: ") && strings.HasSuffix(commName, " [mux]") {
		return true, nil
	}
	if commandName != "ssh" {
		return false, nil
	}
	return strings.Contains(command, "controlpersist=") || strings.Contains(command, "controlmaster=yes") || strings.Contains(command, "controlmaster=auto") || strings.Contains(" "+command+" ", " -m "), nil
}

func processReferencesSandboxOnlyByCWD(pid int, root string) (bool, error) {
	procRoot := filepath.Join("/proc", fmt.Sprint(pid))
	cwd, err := os.Readlink(filepath.Join(procRoot, "cwd"))
	if err != nil {
		return false, err
	}
	if !processPathInsideRoot(cwd, root) {
		return false, nil
	}
	entries, err := os.ReadDir(filepath.Join(procRoot, "fd"))
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		path, err := os.Readlink(filepath.Join(procRoot, "fd", entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if processPathInsideRoot(path, root) {
			return false, nil
		}
	}
	maps, err := os.ReadFile(filepath.Join(procRoot, "maps"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(maps), "\n") {
		if mapped := procMapPath(line); mapped != "" && processPathInsideRoot(mapped, root) {
			return false, nil
		}
	}
	return true, nil
}

func isSharedTaskDaemonCommand(comm string, args []string) bool {
	command := strings.ToLower(strings.Join(args, " "))
	commandName := ""
	if len(args) > 0 {
		commandName = strings.ToLower(filepath.Base(args[0]))
	}
	comm = strings.ToLower(comm)

	switch {
	case comm == "ssh" && strings.HasPrefix(command, "ssh: ") && strings.HasSuffix(command, " [mux]"):
		return true
	case commandName == "ssh":
		return strings.Contains(" "+command+" ", " -m ") || strings.Contains(command, "controlmaster=yes") || strings.Contains(command, "controlmaster=auto") || strings.Contains(command, "controlpersist=")
	case strings.Contains(command, "org.gradle.launcher.daemon.bootstrap.gradledaemon"):
		return true
	case strings.Contains(command, "org.jetbrains.kotlin.daemon.kotlincompiledaemon"):
		return true
	case commandName == "gopls" && strings.Contains(command, "-remote=auto"):
		return true
	case commandName == "sccache":
		return len(args) == 1 || strings.Contains(command, "--start-server") || strings.Contains(command, "server")
	case strings.HasPrefix(comm, "tmux: server"):
		return true
	case commandName == "tmux":
		if len(args) > 1 && strings.EqualFold(args[1], "server") {
			return true
		}
		for _, arg := range args[1:] {
			if arg == "-D" {
				return true
			}
		}
		return false
	case comm == "screen" || strings.HasPrefix(comm, "screen ") || strings.HasPrefix(comm, "screen:"):
		return true
	case commandName == "screen":
		return strings.Contains(command, "-dm") || strings.Contains(command, "-d -m") || strings.Contains(command, "-d -r")
	default:
		return false
	}
}

func describeSharedTaskProcesses(processes []taskProcessDetail) string {
	labels := make([]string, 0, len(processes))
	for _, process := range processes {
		labels = append(labels, fmt.Sprintf("PID %d %s", process.PID, process.Command))
	}
	return strings.Join(labels, "; ")
}
