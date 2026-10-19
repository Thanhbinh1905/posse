package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"syscall"

	"github.com/thanhbinh1905/posse/internal/store"
)

type taskProcessSignalGuard func(int, syscall.Signal) error

type taskProcessDetail struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
	Ports   []int  `json:"ports,omitempty"`
}

func taskProcessPIDSet(pids []int) map[int]bool {
	result := make(map[int]bool, len(pids))
	for _, pid := range pids {
		if pid > 1 {
			result[pid] = true
		}
	}
	return result
}

func mergeTaskProcessExclusions(groups ...map[int]bool) map[int]bool {
	result := map[int]bool{}
	for _, group := range groups {
		for pid := range group {
			result[pid] = true
		}
	}
	return result
}

func taskProcessDetailsPIDSet(processes []taskProcessDetail) map[int]bool {
	pids := make([]int, 0, len(processes))
	for _, process := range processes {
		pids = append(pids, process.PID)
	}
	return taskProcessPIDSet(pids)
}

func ensureTaskContainmentIdentity(ctx context.Context, db *store.DB, task store.Task) (store.Task, error) {
	if task.ContainmentMarker != "" {
		marker, err := hex.DecodeString(task.ContainmentMarker)
		if err != nil || len(marker) != 24 {
			return task, fmt.Errorf("task process marker is invalid")
		}
		return task, nil
	}
	marker := make([]byte, 24)
	if _, err := rand.Read(marker); err != nil {
		return task, fmt.Errorf("generate Task process marker: %w", err)
	}
	task.ContainmentMarker = hex.EncodeToString(marker)
	if err := db.UpdateTaskContainment(ctx, task.ID, task.ContainmentMode, task.ContainmentPath, task.ContainmentMarker); err != nil {
		return task, fmt.Errorf("record Task process containment identity: %w", err)
	}
	return task, nil
}

func (s *Service) setupTaskContainment(ctx context.Context, db *store.DB, project store.Project, task store.Task, paneID, scratch string) (store.Task, error) {
	var err error
	task, err = ensureTaskContainmentIdentity(ctx, db, task)
	if err != nil {
		return task, err
	}
	if s.taskContainmentSetup == nil && task.ContainmentMode == "cgroup" && task.ContainmentPath == "" {
		return task, fmt.Errorf("task is recorded in cgroup mode without a cgroup path")
	}
	if s.taskContainmentSetup == nil && task.ContainmentMode != "marker" && task.ContainmentPath == "" {
		path, available, err := taskCgroupPathForTask(ctx, project, task)
		if err != nil {
			return task, err
		}
		if available {
			task.ContainmentPath = path
			if err := db.UpdateTaskContainment(ctx, task.ID, task.ContainmentMode, path, task.ContainmentMarker); err != nil {
				return task, fmt.Errorf("record Task cgroup path: %w", err)
			}
		}
	}
	mode := ""
	if s.taskContainmentSetup != nil {
		mode, err = s.taskContainmentSetup(ctx, task, paneID, scratch)
	} else {
		mode, err = setupTaskProcessContainment(ctx, s, project, task, paneID, scratch)
	}
	if err != nil {
		return task, err
	}
	if mode != "cgroup" {
		task.ContainmentPath = ""
	}
	task.ContainmentMode = mode
	if err := db.UpdateTaskContainment(ctx, task.ID, mode, task.ContainmentPath, task.ContainmentMarker); err != nil {
		cleanupErr := stopTaskCgroup(ctx, task)
		return task, fmt.Errorf("persist Task process containment mode: %w", errors.Join(err, cleanupErr))
	}
	return task, nil
}
