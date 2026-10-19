package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

const largeSharedGoCacheBytes int64 = 1 << 30

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
		case store.StateSpawning, store.StateWorking, store.StateNeedsDecision, store.StateBlocked, store.StateStalled, store.StateDone, store.StateLanding:
			continue
		}
		processes := taskContainmentProcessDetails(task)
		if task.WorktreePath != "" {
			mountProcesses, mountErr := mountTaskProcessDetails(task.WorktreePath)
			if mountErr == nil {
				processes = mergeTaskProcessDetails(processes, mountProcesses)
			} else {
				problems = append(problems, fmt.Sprintf("t%d process scan: %v", task.Seq, mountErr))
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
		{label: "Remuda", path: filepath.Join(home, "remuda", project.Name)},
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

func sharedGoCacheDiagnostic(ctx context.Context) (status, detail, action string) {
	cachePath := os.Getenv("GOCACHE")
	if cachePath == "" {
		if goBinary, err := exec.LookPath("go"); err == nil {
			commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if output, err := exec.CommandContext(commandCtx, goBinary, "env", "GOCACHE").Output(); err == nil {
				cachePath = strings.TrimSpace(string(output))
			}
		}
	}
	if cachePath == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return "warn", "could not locate the shared Go build cache: " + err.Error(), "Check GOCACHE manually; Posse never deletes shared caches"
		}
		cachePath = filepath.Join(cacheDir, "go-build")
	}
	absolute, err := filepath.Abs(cachePath)
	if err != nil {
		return "warn", "could not resolve GOCACHE: " + err.Error(), "Check GOCACHE manually; Posse never deletes shared caches"
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	bytes, err := pathBytes(absolute)
	if err != nil {
		return "warn", fmt.Sprintf("GOCACHE %s could not be measured: %v", absolute, err), "Check the shared cache path and permissions"
	}
	detail = fmt.Sprintf("GOCACHE uses %d bytes at %s; Posse never removes shared caches", bytes, absolute)
	if bytes >= largeSharedGoCacheBytes {
		return "warn", detail, "Review this shared cache; if safe, run `go clean -cache` manually"
	}
	return "ok", detail, ""
}
