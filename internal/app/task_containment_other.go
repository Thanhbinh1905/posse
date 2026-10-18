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

func stopTaskProcessContainmentWithGuard(context.Context, store.Task, taskProcessSignalGuard) ([]taskProcessDetail, error) {
	return nil, nil
}

func stopTaskCgroup(context.Context, store.Task) error {
	return fmt.Errorf("Task cgroup shutdown is unavailable on this platform")
}

func retireEmptyTaskCgroup(store.Task) error {
	return nil
}

func taskContainmentProcessDetails(store.Task) []taskProcessDetail { return nil }

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

func mountTaskProcessDetails(string) ([]taskProcessDetail, error) {
	return nil, fmt.Errorf("Mount process diagnostics are unavailable on this platform")
}
