//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func stopTaskProcessContainment(ctx context.Context, task store.Task) ([]taskProcessDetail, error) {
	return stopTaskProcessContainmentWithGuard(ctx, task, nil)
}

func stopTaskProcessContainmentWithGuard(ctx context.Context, task store.Task, signalGuard taskProcessSignalGuard) ([]taskProcessDetail, error) {
	preserved, err := detachedSharedTaskProcessPIDs(task)
	if err != nil {
		return taskContainmentProcessDetails(task), err
	}
	if len(preserved) > 0 {
		processes := processDetails(preserved)
		return processes, fmt.Errorf("preserving detached shared daemon processes outside the Rider's parent chain: %s", describeSharedTaskProcesses(processes))
	}
	if task.ContainmentMode == "cgroup" {
		// Herdr can start Task-marked commands outside the pane shell's cgroup.
		var markerErr error
		if task.ContainmentMarker != "" {
			_, markerErr = stopMarkedTaskProcesses(task.ContainmentMarker, signalGuard)
		}
		cgroupErr := stopTaskCgroupWithGuard(ctx, task, signalGuard)
		if markerErr != nil || cgroupErr != nil {
			return taskContainmentProcessDetails(task), errors.Join(markerErr, cgroupErr)
		}
		return nil, nil
	}
	if task.ContainmentMarker == "" {
		return nil, nil
	}
	return stopMarkedTaskProcesses(task.ContainmentMarker, signalGuard)
}

func taskContainmentProcessDetails(task store.Task) []taskProcessDetail {
	if task.ContainmentMode == "cgroup" {
		processes := taskProcessesInCgroup(task.ContainmentPath)
		if task.ContainmentMarker == "" {
			return processes
		}
		pids, err := markedTaskPIDs(task.ContainmentMarker)
		if err != nil {
			return processes
		}
		return mergeTaskProcessDetails(processes, processDetails(pids))
	}
	if task.ContainmentMarker == "" {
		return nil
	}
	pids, err := markedTaskPIDs(task.ContainmentMarker)
	if err != nil {
		return nil
	}
	return processDetails(pids)
}

func mountTaskProcessDetails(path string) ([]taskProcessDetail, error) {
	pids, err := mountProcessIDs(path)
	if err != nil {
		return nil, err
	}
	return processDetails(pids), nil
}

func taskScratchProcessDetails(home string, project store.Project, task store.Task) ([]taskProcessDetail, error) {
	root, err := validatedTaskScratchRoot(home, project, task)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	pids, err := processIDsInRoots([]ownedProcessRoot{{path: root, includeOpenFiles: true, includeMappedFiles: true}})
	if err != nil {
		return nil, err
	}
	return processDetails(pids), nil
}

func processHasTaskMarker(pid int, marker string) bool {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return false
	}
	for _, value := range strings.Split(string(contents), "\x00") {
		if value == "POSSE_TASK_ID="+marker {
			return true
		}
	}
	return false
}

func stopMarkedTaskProcesses(marker string, signalGuard taskProcessSignalGuard) ([]taskProcessDetail, error) {
	handles := map[int]*mountProcessHandle{}
	terminated := map[int]bool{}
	defer func() {
		for _, handle := range handles {
			_ = handle.Close()
		}
	}()
	started := time.Now()
	deadline := started.Add(8 * time.Second)
	for time.Now().Before(deadline) {
		pids, err := markedTaskPIDs(marker)
		if err != nil {
			return nil, err
		}
		for _, pid := range pids {
			if existing := handles[pid]; existing != nil {
				alive, err := existing.Alive()
				if err != nil {
					return processDetails(pids), err
				}
				if alive || !processHasTaskMarker(pid, marker) {
					continue
				}
				bootID, startTime, err := store.ProcessIdentityForPID(pid)
				if err != nil || bootID+"/"+startTime == existing.Identity() {
					continue
				}
				_ = existing.Close()
				delete(handles, pid)
				delete(terminated, pid)
			}
			if !processHasTaskMarker(pid, marker) {
				continue
			}
			handle, err := openMountProcessHandle(pid)
			if err != nil {
				if !processHasTaskMarker(pid, marker) {
					continue // The observed instance exited before its pidfd was opened.
				}
				return processDetails(pids), fmt.Errorf("cannot safely stop marked Task process %d: %w", pid, err)
			}
			if !processHasTaskMarker(pid, marker) {
				_ = handle.Close()
				continue
			}
			if signalGuard != nil {
				handle.signalGuard = func(signal syscall.Signal) error { return signalGuard(pid, signal) }
			}
			handles[pid] = handle
		}
		alive := false
		for pid, handle := range handles {
			isAlive, err := handle.Alive()
			if err != nil {
				return processDetails(pids), err
			}
			if !isAlive {
				continue
			}
			alive = true
			if !terminated[pid] {
				if err := handle.Signal(syscall.SIGTERM); err != nil {
					return processDetails(pids), err
				}
				terminated[pid] = true
			} else if time.Since(started) >= 2*time.Second {
				if err := handle.Signal(syscall.SIGKILL); err != nil {
					return processDetails(pids), err
				}
			}
		}
		if len(pids) == 0 && !alive {
			return nil, nil
		}
		time.Sleep(75 * time.Millisecond)
	}
	remaining, err := markedTaskPIDs(marker)
	if err != nil {
		return nil, err
	}
	return processDetails(remaining), fmt.Errorf("task processes survived marker shutdown: %v", remaining)
}

func markedTaskPIDs(marker string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := []int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 1 && processHasTaskMarker(pid, marker) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids, nil
}

func taskOwnedProcessDetails(pids []int) []taskProcessDetail {
	return processDetails(pids)
}

func processDetails(pids []int) []taskProcessDetail {
	result := make([]taskProcessDetail, 0, len(pids))
	for _, pid := range pids {
		command, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		cmd := strings.Join(strings.Fields(strings.ReplaceAll(string(command), "\x00", " ")), " ")
		if cmd == "" {
			if comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm")); err == nil {
				cmd = strings.TrimSpace(string(comm))
			}
		}
		result = append(result, taskProcessDetail{PID: pid, Command: cmd, Ports: processListeningPorts(pid)})
	}
	return result
}

func processListeningPorts(pid int) []int {
	fdEntries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return nil
	}
	inodes := map[string]bool{}
	for _, entry := range fdEntries {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", entry.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
	}
	ports := map[int]bool{}
	for _, table := range []string{"tcp", "tcp6"} {
		contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "net", table))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(contents), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" || !inodes[fields[9]] {
				continue
			}
			separator := strings.LastIndexByte(fields[1], ':')
			if separator < 0 {
				continue
			}
			port, err := strconv.ParseInt(fields[1][separator+1:], 16, 32)
			if err == nil && port > 0 {
				ports[int(port)] = true
			}
		}
	}
	result := make([]int, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}
	sort.Ints(result)
	return result
}
