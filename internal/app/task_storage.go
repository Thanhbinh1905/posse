package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

const largeSharedGoCacheBytes int64 = 8 << 30

func taskDiskLeakSummary(home string, project store.Project, tasks []store.Task) (string, string) {
	var total int64
	var present int
	sizes := []string{}
	leaks := []string{}
	problems := []string{}
	for _, task := range tasks {
		root, err := validatedTaskScratchRoot(home, project, task)
		if err != nil {
			problems = append(problems, fmt.Sprintf("t%d scratch path: %v", task.Seq, err))
			continue
		}
		bytes, err := pathBytes(root)
		if err != nil {
			problems = append(problems, fmt.Sprintf("t%d scratch size: %v", task.Seq, err))
			continue
		}
		if bytes > 0 {
			present++
			total += bytes
			sizes = append(sizes, fmt.Sprintf("t%d=%d", task.Seq, bytes))
		}
		switch task.State {
		case store.StateSpawning, store.StateWorking, store.StateNeedsDecision, store.StateBlocked, store.StateStalled, store.StateDone, store.StateMerging:
			continue
		}
		processes := taskContainmentProcessDetails(task)
		if task.WorktreePath != "" {
			sandboxProcesses, sandboxErr := sandboxTaskProcessDetails(task.WorktreePath)
			if sandboxErr == nil {
				processes = mergeTaskProcessDetails(processes, sandboxProcesses)
			} else {
				problems = append(problems, fmt.Sprintf("t%d process scan: %v", task.Seq, sandboxErr))
			}
		}
		scratchProcesses, scratchErr := taskScratchProcessDetails(home, project, task)
		if scratchErr != nil {
			problems = append(problems, fmt.Sprintf("t%d scratch process scan: %v", task.Seq, scratchErr))
		} else {
			processes = mergeTaskProcessDetails(processes, scratchProcesses)
		}
		for _, process := range processes {
			label := fmt.Sprintf("t%d PID %d %s", task.Seq, process.PID, process.Command)
			if len(process.Ports) > 0 {
				ports := make([]string, len(process.Ports))
				for index, port := range process.Ports {
					ports[index] = fmt.Sprintf("%d", port)
				}
				label += " listening on " + strings.Join(ports, ",")
			}
			leaks = append(leaks, label)
		}
	}
	directoryLabel := "directories"
	if present == 1 {
		directoryLabel = "directory"
	}
	detail := fmt.Sprintf("%d bytes in %d Task scratch %s", total, present, directoryLabel)
	if len(sizes) > 0 {
		if len(sizes) > 8 {
			sizes = append(sizes[:8], fmt.Sprintf("+%d more", len(sizes)-8))
		}
		detail += " (" + strings.Join(sizes, ", ") + ")"
	}
	if len(leaks) > 0 {
		detail += "; residual processes: " + strings.Join(leaks, "; ")
	}
	if len(problems) > 0 {
		detail += "; inspection issues: " + strings.Join(problems, "; ")
	}
	status := "ok"
	if len(leaks) > 0 || len(problems) > 0 {
		status = "warn"
	}
	return status, detail
}

func projectDiskUsage(home string, project store.Project) (string, error) {
	paths := []struct {
		label string
		path  string
	}{
		{label: "scratch", path: filepath.Join(home, "scratch", project.Name)},
		{label: "Pool", path: poolProjectPath(home, project.Name)},
		{label: "Project data", path: filepath.Join(home, "projects", project.Name)},
	}
	parts := make([]string, 0, len(paths))
	var total int64
	for _, item := range paths {
		bytes, err := pathBytes(item.path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", item.label, err)
		}
		parts = append(parts, fmt.Sprintf("%s=%d bytes", item.label, bytes))
		total += bytes
	}
	return fmt.Sprintf("%d bytes total (%s)", total, strings.Join(parts, ", ")), nil
}

func workerGoBuildCacheDiagnostic(home string, policy workerGoCachePolicy) (status, detail, action string) {
	cachePath := workerGoBuildCachePath(home)
	info, err := os.Lstat(cachePath)
	if os.IsNotExist(err) {
		return "ok", fmt.Sprintf("Worker builds use a Posse-owned shared GOCACHE at %s; it has not been created yet", cachePath), ""
	}
	if err != nil {
		return "warn", fmt.Sprintf("Posse Worker GOCACHE %s could not be inspected: %v", cachePath, err), "Check POSSE_HOME permissions"
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "warn", fmt.Sprintf("Posse Worker GOCACHE path is not a real directory: %s", cachePath), "Repair the cache path under POSSE_HOME before starting more Tasks"
	}
	bytes, err := pathBytes(cachePath)
	if err != nil {
		return "warn", fmt.Sprintf("Posse Worker GOCACHE %s could not be measured: %v", cachePath, err), "Check POSSE_HOME permissions"
	}
	detail = fmt.Sprintf("Posse Worker GOCACHE uses %d bytes at %s; entries unused for %s are trimmed at most every %s once usage reaches %d bytes (worker_go_cache)", bytes, cachePath, policy.minAge, policy.interval, policy.maxSize)
	if bytes >= policy.maxSize {
		return "warn", detail, fmt.Sprintf("Posse trims entries unused for %s within %s; if recent Worker builds keep usage above the limit, raise worker_go_cache.max_size or lower worker_go_cache.min_age", policy.minAge, policy.interval)
	}
	return "ok", detail, ""
}

func sharedGoCacheDiagnostic(ctx context.Context) (status, detail, action string) {
	cachePath, err := locateSharedGoBuildCache(ctx)
	if err != nil {
		return "warn", "could not locate the shared Go build cache: " + err.Error(), "Check GOCACHE manually"
	}
	bytes, err := pathBytes(cachePath)
	if err != nil {
		return "warn", fmt.Sprintf("GOCACHE %s could not be measured: %v", cachePath, err), "Check the shared cache path and permissions"
	}
	detail = fmt.Sprintf("User GOCACHE uses %d bytes at %s; Worker builds use a separate Posse-owned cache", bytes, cachePath)
	if bytes >= largeSharedGoCacheBytes {
		return "warn", detail, "Posse does not alter the User cache; after Go builds finish, review it and run `go clean -cache` manually if desired"
	}
	return "ok", detail, ""
}
