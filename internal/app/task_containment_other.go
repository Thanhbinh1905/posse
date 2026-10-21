//go:build !linux

package app

import (
	"context"
	"fmt"

	"github.com/thanhbinh1905/posse/internal/store"
)

func taskCgroupPathForTask(context.Context, store.Project, store.Task) (string, bool, error) {
	return "", false, nil
}

func setupTaskProcessContainment(context.Context, *Service, store.Project, store.Task, string, string) (string, error) {
	return "marker", nil
}

func stopTaskProcessContainment(context.Context, store.Task) ([]taskProcessDetail, error) {
	return nil, nil
}

func stopTaskProcessContainmentWithGuard(context.Context, store.Task, taskProcessSignalGuard, ...int) ([]taskProcessDetail, error) {
	return nil, nil
}

func stopTaskCgroup(context.Context, store.Task) error {
	return fmt.Errorf("Task cgroup shutdown is unavailable on this platform")
}

func retireEmptyTaskCgroup(store.Task) error {
	return nil
}

func retireEmptyTaskCgroupWithPreserved(store.Task, map[int]bool) error {
	return nil
}

func taskContainmentProcessDetails(store.Task) []taskProcessDetail { return nil }

func retainTaskContainmentProcesses(*sandboxProcessAuthorization, store.Task, []ownedProcessRoot) error {
	return nil
}

func processHasTaskMarker(int, string) bool { return false }

func taskSharedProcessExclusions(store.Task) ([]taskProcessDetail, map[int]bool, error) {
	return nil, map[int]bool{}, nil
}

func describeSharedTaskProcesses([]taskProcessDetail) string { return "" }

func taskProcessHasExcludedAncestor(pid int, excluded map[int]bool) (bool, error) {
	return excluded[pid], nil
}

func excludeTaskProcessTree(pids []int, excluded map[int]bool) ([]int, error) {
	if len(excluded) == 0 {
		return pids, nil
	}
	result := make([]int, 0, len(pids))
	for _, pid := range pids {
		if !excluded[pid] {
			result = append(result, pid)
		}
	}
	return result, nil
}

func taskScratchProcessDetails(string, store.Project, store.Task) ([]taskProcessDetail, error) {
	return nil, nil
}

func taskOwnedProcessDetails(pids []int) []taskProcessDetail {
	result := make([]taskProcessDetail, 0, len(pids))
	for _, pid := range pids {
		result = append(result, taskProcessDetail{PID: pid})
	}
	return result
}

func sandboxTaskProcessDetails(string) ([]taskProcessDetail, error) {
	return nil, fmt.Errorf("Sandbox process diagnostics are unavailable on this platform")
}
