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

func stopTaskProcessContainmentWithGuard(ctx context.Context, task store.Task, signalGuard taskProcessSignalGuard, paneShellPID ...int) ([]taskProcessDetail, error) {
	preserved, err := detachedSharedTaskProcessPIDs(task)
	if err != nil {
		return taskContainmentProcessDetails(task), err
	}
	excluded := taskProcessPIDSet(preserved)
	preservedDetails := processDetails(preserved)
	shellPID := 0
	if len(paneShellPID) > 0 {
		shellPID = paneShellPID[0]
	}
	if task.ContainmentMode == "cgroup" {
		// Herdr can start Task-marked commands outside the pane shell's cgroup.
		var markerErr error
		if task.ContainmentMarker != "" {
			// Leave the pane shell for the cgroup pass, where it is signalled
			// after every other cgroup member. The final marker pass catches a
			// shell that is not in the cgroup and signals it last there.
			markerExcluded := excluded
			if shellPID > 1 {
				markerExcluded = mergeTaskProcessExclusions(excluded, taskProcessPIDSet([]int{shellPID}))
			}
			_, markerErr = stopMarkedTaskProcesses(task.ContainmentMarker, signalGuard, markerExcluded, 0)
		}
		cgroupErr := stopTaskCgroupWithGuard(ctx, task, signalGuard, excluded, shellPID)
		if markerErr != nil || cgroupErr != nil {
			return taskContainmentProcessDetails(task), errors.Join(markerErr, cgroupErr)
		}
		if task.ContainmentMarker != "" {
			_, markerErr = stopMarkedTaskProcesses(task.ContainmentMarker, signalGuard, excluded, shellPID)
			if markerErr != nil {
				return taskContainmentProcessDetails(task), markerErr
			}
		}
		return preservedDetails, nil
	}
	if task.ContainmentMarker == "" {
		return preservedDetails, nil
	}
	_, stopErr := stopMarkedTaskProcesses(task.ContainmentMarker, signalGuard, excluded, shellPID)
	return preservedDetails, stopErr
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

func retainTaskContainmentProcesses(authorization *sandboxProcessAuthorization, task store.Task, roots []ownedProcessRoot) error {
	if authorization == nil {
		return fmt.Errorf("task process authorization is unavailable")
	}
	pids := map[int]bool{}
	if task.ContainmentMode == "cgroup" && task.ContainmentPath != "" {
		if err := validateTaskCgroupPath(task.ContainmentPath, task); err != nil {
			return fmt.Errorf("verify Task cgroup before retaining its processes: %w", err)
		}
		cgroupPIDs, err := cgroupProcesses(task.ContainmentPath)
		if err != nil {
			return fmt.Errorf("enumerate Task cgroup processes: %w", err)
		}
		for _, pid := range cgroupPIDs {
			pids[pid] = true
		}
	}
	if task.ContainmentMarker != "" {
		markedPIDs, err := markedTaskPIDs(task.ContainmentMarker)
		if err != nil {
			return fmt.Errorf("enumerate Task-marked processes: %w", err)
		}
		for _, pid := range markedPIDs {
			pids[pid] = true
		}
	}
	ordered := make([]int, 0, len(pids))
	for pid := range pids {
		if pid > 1 {
			ordered = append(ordered, pid)
		}
	}
	sort.Ints(ordered)
	for _, pid := range ordered {
		if !taskContainsProcess(pid, task, roots) {
			continue
		}
		bootID, startTime, err := store.ProcessIdentityForPID(pid)
		if processGone(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect Task-contained process %d: %w", pid, err)
		}
		if !taskContainsProcess(pid, task, roots) {
			if taskProcessExited(pid) {
				continue
			}
			return fmt.Errorf("task process %d left its containment identity during ownership proof", pid)
		}
		retained, err := retainTaskProcessHandle(pid, bootID+"/"+startTime, task, roots, authorization)
		if err != nil {
			return fmt.Errorf("retain Task-contained process %d: %w", pid, err)
		}
		if !retained {
			continue
		}
		processPID := pid
		authorization.setSignalGuard(processPID, func(syscall.Signal) error {
			stillAlive, err := authorization.verifyIfAlive(processPID)
			if err != nil {
				return err
			}
			if stillAlive {
				owned, err := taskProcessHasOwnershipProof(processPID, task, roots, authorization)
				if err != nil {
					return err
				}
				if !owned {
					return fmt.Errorf("%w: PID %d", errTaskProcessOutsideOwnedPaths, processPID)
				}
			}
			return nil
		})
	}
	authorization.authorizeTaskProcess = func(pid int) error {
		zombie, err := sandboxProcessIsZombie(pid)
		if processGone(err) || err == nil && zombie {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect newly observed task process %d state: %w", pid, err)
		}
		owned, err := taskProcessHasOwnershipProof(pid, task, roots, authorization)
		if err != nil {
			return fmt.Errorf("verify newly observed Task process %d ownership: %w", pid, err)
		}
		if !owned {
			alive, err := unretainedSandboxProcessAlive(pid)
			if processGone(err) || err == nil && !alive {
				return nil
			}
			if err != nil {
				return fmt.Errorf("verify newly observed task process %d: %w", pid, err)
			}
			return fmt.Errorf("%w: PID %d", errTaskProcessOutsideOwnedPaths, pid)
		}
		bootID, startTime, err := store.ProcessIdentityForPID(pid)
		if processGone(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect newly observed Task process %d: %w", pid, err)
		}
		handle, err := authorization.bind(pid)
		if processGone(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("retain newly observed Task process %d: %w", pid, err)
		}
		if handle.Identity() != bootID+"/"+startTime {
			if authorization.handles[pid] == handle {
				delete(authorization.handles, pid)
			}
			_ = handle.Close()
			return fmt.Errorf("newly observed Task process %d changed containment identity while retaining its handle", pid)
		}
		alive, err := verifySandboxProcessHandleIfAlive(pid, handle)
		if err != nil {
			return fmt.Errorf("recheck newly observed Task process %d identity: %w", pid, err)
		}
		if alive {
			owned, err := taskProcessHasOwnershipProof(pid, task, roots, authorization)
			if err != nil {
				return fmt.Errorf("recheck newly observed Task process %d ownership: %w", pid, err)
			}
			if !owned {
				return fmt.Errorf("%w: PID %d", errTaskProcessOutsideOwnedPaths, pid)
			}
		}
		authorization.setSignalGuard(pid, func(syscall.Signal) error {
			alive, err := verifySandboxProcessHandleIfAlive(pid, handle)
			if err != nil {
				return err
			}
			if alive {
				owned, err := taskProcessHasOwnershipProof(pid, task, roots, authorization)
				if err != nil {
					return err
				}
				if !owned {
					return fmt.Errorf("%w: PID %d", errTaskProcessOutsideOwnedPaths, pid)
				}
			}
			return nil
		})
		return nil
	}
	return nil
}

func retainTaskProcessHandle(pid int, observedIdentity string, task store.Task, roots []ownedProcessRoot, authorization *sandboxProcessAuthorization) (bool, error) {
	if authorization == nil {
		return false, fmt.Errorf("task process authorization is unavailable")
	}
	for attempt := 0; attempt < 3; attempt++ {
		handle, err := authorization.bind(pid)
		if processGone(err) {
			return false, nil
		}
		if errors.Is(err, errSandboxProcessChangedWhileBinding) {
			owned, proofErr := taskProcessHasOwnershipProof(pid, task, roots, authorization)
			if proofErr != nil || !owned {
				return false, proofErr
			}
			continue
		}
		if err != nil {
			return false, err
		}
		if handle.Identity() != observedIdentity {
			alive, err := handle.Alive()
			if err != nil {
				return false, fmt.Errorf("verify Task process %d handle after identity changed: %w", pid, err)
			}
			if !alive {
				if authorization.handles[pid] == handle {
					delete(authorization.handles, pid)
				}
				_ = handle.Close()
				owned, err := taskProcessHasOwnershipProof(pid, task, roots, authorization)
				if err != nil || !owned {
					return false, err
				}
				continue
			}
			owned, err := taskProcessHasOwnershipProof(pid, task, roots, authorization)
			if err != nil {
				return false, err
			}
			if !owned {
				if authorization.handles[pid] == handle {
					delete(authorization.handles, pid)
				}
				_ = handle.Close()
				return false, nil
			}
			alive, err = verifySandboxProcessHandleIfAlive(pid, handle)
			if err != nil {
				return false, fmt.Errorf("recheck Task process %d after identity changed: %w", pid, err)
			}
			if !alive {
				if authorization.handles[pid] == handle {
					delete(authorization.handles, pid)
				}
				_ = handle.Close()
				return false, nil
			}
		}
		alive, err := authorization.verifyIfAlive(pid)
		if err != nil {
			return false, err
		}
		if alive && !taskContainsProcess(pid, task, roots) {
			if taskProcessExited(pid) {
				return false, nil
			}
			return false, fmt.Errorf("task process %d left its containment identity during ownership proof", pid)
		}
		return alive, nil
	}
	return false, fmt.Errorf("task process %d kept changing identity while retaining its handle", pid)
}

// taskProcessExited reports whether pid is gone or a zombie. Containment reads
// fail for such a process, so a failed read is only an escape when it is alive.
func taskProcessExited(pid int) bool {
	zombie, err := sandboxProcessIsZombie(pid)
	return processGone(err) || err == nil && zombie
}

func taskProcessHasOwnershipProof(pid int, task store.Task, roots []ownedProcessRoot, authorization *sandboxProcessAuthorization) (bool, error) {
	if taskContainsProcess(pid, task, roots) {
		return true, nil
	}
	if task.ContainmentMarker == "" || !processHasTaskMarker(pid, task.ContainmentMarker) {
		return false, nil
	}
	return hasRetainedSandboxProcessAncestor(pid, authorization)
}

func taskContainsProcess(pid int, task store.Task, roots []ownedProcessRoot) bool {
	if task.ContainmentMode == "cgroup" && task.ContainmentPath != "" && processInCgroup(pid, task.ContainmentPath) {
		return true
	}
	if task.ContainmentMarker == "" || !processHasTaskMarker(pid, task.ContainmentMarker) {
		return false
	}
	references, _ := processRootReferences(pid, roots)
	return references
}

func excludeTaskProcessTree(pids []int, excluded map[int]bool) ([]int, error) {
	if len(excluded) == 0 || len(pids) == 0 {
		return pids, nil
	}
	result := make([]int, 0, len(pids))
	for _, pid := range pids {
		excludedProcess, err := taskProcessHasExcludedAncestor(pid, excluded)
		if err != nil {
			return nil, fmt.Errorf("verify whether process %d descends from preserved shared infrastructure: %w", pid, err)
		}
		if !excludedProcess {
			result = append(result, pid)
		}
	}
	return result, nil
}

func taskProcessHasExcludedAncestor(pid int, excluded map[int]bool) (bool, error) {
	current := pid
	seen := map[int]bool{}
	for range 1024 {
		if excluded[current] {
			return true, nil
		}
		if seen[current] {
			return false, fmt.Errorf("process ancestry loop at PID %d", current)
		}
		seen[current] = true
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
		current = parent
	}
	return false, fmt.Errorf("process ancestry for PID %d exceeded 1024 parents", pid)
}

func sandboxTaskProcessDetails(path string) ([]taskProcessDetail, error) {
	pids, err := sandboxProcessIDs(path)
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

func stopMarkedTaskProcesses(marker string, signalGuard taskProcessSignalGuard, excluded map[int]bool, paneShellPID int) ([]taskProcessDetail, error) {
	handles := map[int]*sandboxProcessHandle{}
	terminated := map[int]bool{}
	terminatedAt := map[int]time.Time{}
	ignoredUnowned := map[int]string{}
	defer func() {
		for _, handle := range handles {
			_ = handle.Close()
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	excludedPIDs := excluded
	skipPaneShell := false
	for time.Now().Before(deadline) {
		pids, err := markedTaskPIDs(marker)
		if err != nil {
			return nil, err
		}
		if paneShellPID > 1 && !skipPaneShell {
			pids = append(pids, paneShellPID)
			sort.Ints(pids)
			pids = uniqueProcessIDs(pids)
		}
		pids, err = excludeTaskProcessTree(pids, excludedPIDs)
		if err != nil {
			return nil, err
		}
		pids, err = filterIgnoredUnownedTaskProcesses(pids, marker, ignoredUnowned)
		if err != nil {
			return nil, err
		}
		paneShellSkippedThisPass := false
		for _, pid := range pids {
			paneShell := pid == paneShellPID && paneShellPID > 1
			if paneShell && signalGuard != nil && !processHasTaskMarker(pid, marker) {
				if err := signalGuard(pid, 0); err != nil {
					if forgetStalePaneShellProcess(pid, paneShellPID, marker, handles, terminated, err) {
						skipPaneShell = true
						paneShellSkippedThisPass = true
						continue
					}
					if errors.Is(err, errTaskProcessOutsideOwnedPaths) {
						ignoreUnownedTaskProcess(pid, handles, terminated, terminatedAt, ignoredUnowned)
						pids = removeTaskProcessPID(pids, pid)
						continue
					}
					return processDetails(pids), err
				}
			}
			if existing := handles[pid]; existing != nil {
				alive, err := existing.Alive()
				if err != nil {
					return processDetails(pids), err
				}
				if alive || !paneShell && !processHasTaskMarker(pid, marker) {
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
			if !paneShell && !processHasTaskMarker(pid, marker) {
				continue
			}
			handle, err := openSandboxProcessHandle(pid)
			if err != nil {
				if processGone(err) || !paneShell && !processHasTaskMarker(pid, marker) {
					continue // The observed instance exited before its pidfd was opened.
				}
				return processDetails(pids), fmt.Errorf("cannot safely stop marked Task process %d: %w", pid, err)
			}
			if !paneShell && !processHasTaskMarker(pid, marker) {
				_ = handle.Close()
				continue
			}
			if signalGuard != nil {
				handle.signalGuard = func(signal syscall.Signal) error { return signalGuard(pid, signal) }
			}
			handles[pid] = handle
		}
		if paneShellSkippedThisPass {
			pids = removeTaskProcessPID(pids, paneShellPID)
		}
		alive := false
		for _, pid := range taskProcessSignalOrder(handles, paneShellPID) {
			handle := handles[pid]
			isAlive, err := handle.Alive()
			if err != nil {
				return processDetails(pids), err
			}
			if !isAlive {
				continue
			}
			if !terminated[pid] {
				if err := handle.Signal(syscall.SIGTERM); err != nil {
					if forgetStalePaneShellProcess(pid, paneShellPID, marker, handles, terminated, err) {
						skipPaneShell = true
						pids = removeTaskProcessPID(pids, pid)
						continue
					}
					if errors.Is(err, errTaskProcessOutsideOwnedPaths) {
						ignoreUnownedTaskProcess(pid, handles, terminated, terminatedAt, ignoredUnowned)
						pids = removeTaskProcessPID(pids, pid)
						continue
					}
					return processDetails(pids), err
				}
				terminated[pid] = true
				terminatedAt[pid] = time.Now()
			} else {
				grace := 2 * time.Second
				if pid == paneShellPID {
					grace = 250 * time.Millisecond
				}
				if time.Since(terminatedAt[pid]) >= grace {
					if err := handle.Signal(syscall.SIGKILL); err != nil {
						if forgetStalePaneShellProcess(pid, paneShellPID, marker, handles, terminated, err) {
							skipPaneShell = true
							pids = removeTaskProcessPID(pids, pid)
							continue
						}
						if errors.Is(err, errTaskProcessOutsideOwnedPaths) {
							ignoreUnownedTaskProcess(pid, handles, terminated, terminatedAt, ignoredUnowned)
							pids = removeTaskProcessPID(pids, pid)
							continue
						}
						return processDetails(pids), err
					}
				}
			}
			alive = true
		}
		otherPIDs := 0
		for _, pid := range pids {
			if pid != paneShellPID {
				otherPIDs++
			}
		}
		if otherPIDs == 0 && !alive {
			return nil, nil
		}
		time.Sleep(75 * time.Millisecond)
	}
	remaining, err := markedTaskPIDs(marker)
	if err != nil {
		return nil, err
	}
	remaining, err = excludeTaskProcessTree(remaining, excludedPIDs)
	if err != nil {
		return nil, err
	}
	remaining, err = filterIgnoredUnownedTaskProcesses(remaining, marker, ignoredUnowned)
	if err != nil {
		return nil, err
	}
	if shellHandle := handles[paneShellPID]; paneShellPID > 1 && shellHandle != nil {
		alive, err := shellHandle.Alive()
		if err != nil {
			return processDetails(remaining), err
		}
		if alive {
			remaining = append(remaining, paneShellPID)
			sort.Ints(remaining)
			remaining = uniqueProcessIDs(remaining)
		}
	}
	return processDetails(remaining), fmt.Errorf("task processes survived marker shutdown: %v", remaining)
}

func ignoreUnownedTaskProcess(pid int, handles map[int]*sandboxProcessHandle, terminated map[int]bool, terminatedAt map[int]time.Time, ignored map[int]string) {
	identity := ""
	if handle := handles[pid]; handle != nil {
		identity = handle.Identity()
		_ = handle.Close()
		delete(handles, pid)
	}
	if identity == "" {
		bootID, startTime, err := store.ProcessIdentityForPID(pid)
		if err == nil {
			identity = bootID + "/" + startTime
		}
	}
	if identity != "" {
		ignored[pid] = identity
	}
	delete(terminated, pid)
	delete(terminatedAt, pid)
}

func filterIgnoredUnownedTaskProcesses(pids []int, marker string, ignored map[int]string) ([]int, error) {
	filtered := make([]int, 0, len(pids))
	for _, pid := range pids {
		identity, found := ignored[pid]
		if !found {
			filtered = append(filtered, pid)
			continue
		}
		bootID, startTime, err := store.ProcessIdentityForPID(pid)
		if processGone(err) {
			delete(ignored, pid)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("verify ignored Task process %d identity: %w", pid, err)
		}
		if identity == bootID+"/"+startTime && processHasTaskMarker(pid, marker) {
			continue
		}
		delete(ignored, pid)
		filtered = append(filtered, pid)
	}
	return filtered, nil
}

func forgetStalePaneShellProcess(pid, paneShellPID int, marker string, handles map[int]*sandboxProcessHandle, terminated map[int]bool, err error) bool {
	if pid != paneShellPID || paneShellPID <= 1 || processHasTaskMarker(pid, marker) || !errors.Is(err, errPaneShellNotRecorded) {
		return false
	}
	if handle := handles[pid]; handle != nil {
		_ = handle.Close()
		delete(handles, pid)
	}
	delete(terminated, pid)
	return true
}

func removeTaskProcessPID(pids []int, pid int) []int {
	filtered := pids[:0]
	for _, candidate := range pids {
		if candidate != pid {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func uniqueProcessIDs(pids []int) []int {
	if len(pids) < 2 {
		return pids
	}
	unique := pids[:1]
	for _, pid := range pids[1:] {
		if pid != unique[len(unique)-1] {
			unique = append(unique, pid)
		}
	}
	return unique
}

func taskProcessSignalOrder(handles map[int]*sandboxProcessHandle, paneShellPID int) []int {
	pids := make([]int, 0, len(handles))
	for pid := range handles {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	if paneShellPID > 1 {
		for index, pid := range pids {
			if pid == paneShellPID {
				copy(pids[index:], pids[index+1:])
				pids[len(pids)-1] = paneShellPID
				break
			}
		}
	}
	return pids
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
