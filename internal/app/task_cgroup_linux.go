//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

const cgroupV2Root = "/sys/fs/cgroup"

func taskCgroupsAvailable(ctx context.Context) bool {
	if _, err := os.Stat(filepath.Join(cgroupV2Root, "cgroup.controllers")); err != nil || !userSystemdAvailable(ctx) {
		return false
	}
	_, err := userAppSlicePath(ctx)
	return err == nil
}

func userSystemdAvailable(ctx context.Context) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return exec.CommandContext(checkCtx, "systemctl", "--user", "show-environment").Run() == nil
}

func taskCgroupPathForTask(ctx context.Context, _ store.Project, task store.Task) (string, bool, error) {
	if task.ContainmentMode == "marker" || task.ContainmentMarker == "" || !taskCgroupsAvailable(ctx) {
		return "", false, nil
	}
	if len(task.ContainmentMarker) != 48 {
		return "", false, fmt.Errorf("invalid Task process marker")
	}
	base, err := userAppSlicePath(ctx)
	if err != nil {
		return "", false, nil
	}
	name := fmt.Sprintf("posse-task-t%d-%s.scope", task.ID, task.ContainmentMarker[:12])
	path := filepath.Join(base, name)
	if err := validateTaskCgroupPath(path, task); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func userAppSlicePath(ctx context.Context) (string, error) {
	showCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(showCtx, "systemctl", "--user", "show", "--property=ControlGroup", "--value", "app.slice").Output()
	if err != nil {
		return "", err
	}
	controlGroup := strings.TrimSpace(string(output))
	if !strings.HasPrefix(controlGroup, "/user.slice/") || !strings.HasSuffix(controlGroup, "/app.slice") || strings.Contains(controlGroup, "..") {
		return "", fmt.Errorf("unexpected user app.slice ControlGroup %q", controlGroup)
	}
	path := filepath.Join(cgroupV2Root, strings.TrimPrefix(controlGroup, "/"))
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("user app.slice cgroup is unavailable: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o200 == 0 {
		return "", fmt.Errorf("user app.slice cgroup is not delegated to the current user")
	}
	return path, nil
}

func setupTaskProcessContainment(ctx context.Context, service *Service, _ store.Project, task store.Task, paneID, _ string) (string, error) {
	if task.ContainmentMode == "marker" || task.ContainmentPath == "" {
		return "marker", nil
	}
	if err := validateTaskCgroupPath(task.ContainmentPath, task); err != nil {
		return "", err
	}
	pane, err := taskPaneProcessInfo(ctx, service, paneID)
	if err != nil {
		return "", fmt.Errorf("inspect Task pane before cgroup setup: %w", err)
	}
	if pane.ShellPID < 2 {
		return "", fmt.Errorf("task pane has no valid shell PID")
	}
	if err := os.Mkdir(task.ContainmentPath, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
			return "marker", nil
		}
		return "", fmt.Errorf("create Task cgroup: %w", err)
	}
	info, err := os.Stat(task.ContainmentPath)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("task cgroup path is not a directory: %w", err)
	}
	pids, err := cgroupProcesses(task.ContainmentPath)
	if err != nil {
		return "", fmt.Errorf("inspect Task cgroup before attaching Worker shell: %w", err)
	}
	for _, pid := range pids {
		if !taskCgroupMemberBelongsToTask(pid, pane.ShellPID, task.ContainmentPath) {
			return "", fmt.Errorf("task cgroup contains unowned process %d", pid)
		}
	}
	if !processInCgroup(pane.ShellPID, task.ContainmentPath) {
		file, err := os.OpenFile(filepath.Join(task.ContainmentPath, "cgroup.procs"), os.O_WRONLY, 0)
		if err != nil {
			return fallbackToMarkerIfUnmoved(pane.ShellPID, task.ContainmentPath, err)
		}
		_, writeErr := fmt.Fprintf(file, "%d\n", pane.ShellPID)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			if processInCgroup(pane.ShellPID, task.ContainmentPath) {
				return "cgroup", nil
			}
			return fallbackToMarkerIfUnmoved(pane.ShellPID, task.ContainmentPath, errors.Join(writeErr, closeErr))
		}
	}
	if !processInCgroup(pane.ShellPID, task.ContainmentPath) {
		return "", fmt.Errorf("worker shell %d did not enter its Task cgroup", pane.ShellPID)
	}
	return "cgroup", nil
}

type taskPaneInfo struct {
	ShellPID        int
	ForegroundGroup int
}

func taskPaneProcessInfo(ctx context.Context, service *Service, paneID string) (taskPaneInfo, error) {
	raw, err := service.herdrCall(ctx, "pane.process_info", map[string]any{"pane_id": paneID})
	if err != nil {
		return taskPaneInfo{}, err
	}
	var response struct {
		ProcessInfo struct {
			ShellPID        int `json:"shell_pid"`
			ForegroundGroup int `json:"foreground_process_group_id"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return taskPaneInfo{}, err
	}
	if response.ProcessInfo.ShellPID < 2 {
		return taskPaneInfo{}, fmt.Errorf("pane has no valid shell PID")
	}
	return taskPaneInfo{ShellPID: response.ProcessInfo.ShellPID, ForegroundGroup: response.ProcessInfo.ForegroundGroup}, nil
}

func fallbackToMarkerIfUnmoved(pid int, cgroupPath string, err error) (string, error) {
	if processInCgroup(pid, cgroupPath) {
		return "cgroup", nil
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EPERM) {
		_ = os.Remove(cgroupPath)
		return "marker", nil
	}
	return "", fmt.Errorf("attach Worker shell to Task cgroup: %w", err)
}

func validateTaskCgroupPath(path string, task store.Task) error {
	if task.ID < 1 || len(task.ContainmentMarker) < 12 || filepath.Clean(path) != path || !filepath.IsAbs(path) {
		return fmt.Errorf("invalid Task cgroup identity")
	}
	prefix := filepath.Join(cgroupV2Root, "user.slice", fmt.Sprintf("user-%d.slice", os.Getuid()), fmt.Sprintf("user@%d.service", os.Getuid()), "app.slice") + string(filepath.Separator)
	wantName := fmt.Sprintf("posse-task-t%d-%s.scope", task.ID, task.ContainmentMarker[:12])
	if !strings.HasPrefix(path, prefix) || filepath.Base(path) != wantName {
		return fmt.Errorf("task cgroup path does not match its recorded identity")
	}
	return nil
}

func taskCgroupMemberBelongsToTask(pid, paneShellPID int, cgroupPath string) bool {
	return pid == paneShellPID || processInCgroup(pid, cgroupPath)
}

func taskCgroupRelativePath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, cgroupV2Root+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid Task cgroup path %q", path)
	}
	relative, err := filepath.Rel(cgroupV2Root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("task cgroup path escapes cgroup v2 root")
	}
	return string(filepath.Separator) + filepath.ToSlash(relative), nil
}

func processCgroup(pid int) string {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(line, "0::") {
			return strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		}
	}
	return ""
}

func processInCgroup(pid int, cgroupPath string) bool {
	relative, err := taskCgroupRelativePath(cgroupPath)
	current := processCgroup(pid)
	return err == nil && (current == relative || strings.HasPrefix(current, strings.TrimSuffix(relative, "/")+"/"))
}

func cgroupProcesses(cgroupPath string) ([]int, error) {
	if _, err := taskCgroupRelativePath(cgroupPath); err != nil {
		return nil, err
	}
	pids := map[int]bool{}
	err := filepath.WalkDir(cgroupPath, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "cgroup.procs" {
			return nil
		}
		contents, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, line := range strings.Fields(string(contents)) {
			pid, err := strconv.Atoi(line)
			if err != nil || pid < 2 {
				return fmt.Errorf("invalid PID in %s", path)
			}
			pids[pid] = true
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]int, 0, len(pids))
	for pid := range pids {
		result = append(result, pid)
	}
	sort.Ints(result)
	return result, nil
}

func retireEmptyTaskCgroup(task store.Task) error {
	return retireEmptyTaskCgroupWithPreserved(task, nil)
}

func retireEmptyTaskCgroupWithPreserved(task store.Task, preserved map[int]bool) error {
	if task.ContainmentPath == "" {
		return nil
	}
	if err := validateTaskCgroupPath(task.ContainmentPath, task); err != nil {
		return err
	}
	pids, err := cgroupProcesses(task.ContainmentPath)
	if err != nil {
		return err
	}
	if len(pids) > 0 {
		if preserved == nil {
			preservedPIDs, err := detachedSharedTaskProcessPIDs(task)
			if err != nil {
				return err
			}
			preserved = taskProcessPIDSet(preservedPIDs)
		}
		for _, pid := range pids {
			if !preserved[pid] {
				return fmt.Errorf("task cgroup still contains processes outside the final preserved set: %v", pids)
			}
		}
		// A detached shared daemon remains in this Task cgroup until it exits.
		// Keep the emptying/reap path available to `posse clean` afterward.
		return nil
	}
	return removeEmptyTaskCgroup(task.ContainmentPath)
}

func stopTaskCgroup(ctx context.Context, task store.Task) error {
	preserved, err := detachedSharedTaskProcessPIDs(task)
	if err != nil {
		return err
	}
	if err := stopTaskCgroupWithGuard(ctx, task, nil, taskProcessPIDSet(preserved), 0); err != nil {
		return err
	}
	if len(preserved) > 0 {
		processes := processDetails(preserved)
		return fmt.Errorf("preserving detached shared daemon processes outside the Worker's parent chain: %s", describeSharedTaskProcesses(processes))
	}
	return nil
}

var openCgroupProcessHandle = openSandboxProcessHandle

func stopTaskCgroupWithGuard(_ context.Context, task store.Task, signalGuard taskProcessSignalGuard, excluded map[int]bool, paneShellPID int) error {
	if task.ContainmentPath == "" {
		return nil
	}
	if err := validateTaskCgroupPath(task.ContainmentPath, task); err != nil {
		return err
	}
	excludedPIDs := excluded
	if _, err := os.Stat(task.ContainmentPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	handles := map[int]*sandboxProcessHandle{}
	terminated := map[int]bool{}
	terminatedAt := map[int]time.Time{}
	defer func() {
		for _, handle := range handles {
			_ = handle.Close()
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		allPIDs, err := cgroupProcesses(task.ContainmentPath)
		if err != nil {
			return fmt.Errorf("inspect Task cgroup: %w", err)
		}
		pids, err := excludeTaskProcessTree(allPIDs, excludedPIDs)
		if err != nil {
			return err
		}
		for _, pid := range pids {
			if !processInCgroup(pid, task.ContainmentPath) {
				continue
			}
			if handle := handles[pid]; handle != nil {
				alive, err := handle.Alive()
				if err != nil {
					return err
				}
				if alive {
					continue
				}
				bootID, startTime, err := store.ProcessIdentityForPID(pid)
				if err != nil || bootID+"/"+startTime == handle.Identity() {
					continue
				}
				_ = handle.Close()
				delete(handles, pid)
				delete(terminated, pid)
			}
			handle, err := openCgroupProcessHandle(pid)
			if err != nil {
				if processGone(err) || !processInCgroup(pid, task.ContainmentPath) {
					continue // Exited after the listing; the next pass rescans.
				}
				return fmt.Errorf("cannot safely stop Task cgroup process %d: %w", pid, err)
			}
			if !processInCgroup(pid, task.ContainmentPath) {
				_ = handle.Close()
				continue
			}
			if signalGuard != nil {
				handle.signalGuard = func(signal syscall.Signal) error { return signalGuard(pid, signal) }
			}
			handles[pid] = handle
		}
		alive := false
		for _, pid := range taskProcessSignalOrder(handles, paneShellPID) {
			handle := handles[pid]
			isAlive, err := handle.Alive()
			if err != nil {
				return err
			}
			if !isAlive {
				continue
			}
			alive = true
			if !terminated[pid] {
				if err := handle.Signal(syscall.SIGTERM); err != nil {
					return err
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
						return err
					}
				}
			}
		}
		if len(pids) == 0 && !alive {
			if len(allPIDs) > 0 {
				return nil // Only intentionally preserved shared daemons remain in the Task cgroup.
			}
			return removeEmptyTaskCgroup(task.ContainmentPath)
		}
		time.Sleep(75 * time.Millisecond)
	}
	remaining, err := cgroupProcesses(task.ContainmentPath)
	if err != nil {
		return err
	}
	remaining, err = excludeTaskProcessTree(remaining, excludedPIDs)
	if err != nil {
		return err
	}
	return fmt.Errorf("task cgroup processes survived shutdown: %v", remaining)
}

func removeEmptyTaskCgroup(path string) error {
	var directories []string
	err := filepath.WalkDir(path, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			directories = append(directories, current)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Remove(directories[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty Task cgroup %s: %w", directories[i], err)
		}
	}
	return nil
}

func taskProcessesInCgroup(path string) []taskProcessDetail {
	pids, err := cgroupProcesses(path)
	if err != nil {
		return nil
	}
	return processDetails(pids)
}
