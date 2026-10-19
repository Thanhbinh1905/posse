package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) recordTeardownBlocked(ctx context.Context, db *store.DB, project store.Project, task store.Task, cause error) error {
	processes := taskContainmentProcessDetails(task)
	if task.WorktreePath != "" {
		mountProcesses, err := mountTaskProcessDetails(task.WorktreePath)
		if err == nil {
			processes = mergeTaskProcessDetails(processes, mountProcesses)
		}
	}
	if home, err := s.homePath(); err == nil {
		if roots, rootErr := taskOwnedProcessRoots(ctx, db, home, project, task); rootErr == nil {
			if pids, pidErr := processIDsInRoots(roots); pidErr == nil {
				processes = mergeTaskProcessDetails(processes, taskOwnedProcessDetails(pids))
			}
		}
	}
	if len(processes) == 0 {
		return nil
	}
	labels := make([]string, 0, len(processes))
	for _, process := range processes {
		label := fmt.Sprintf("PID %d %s", process.PID, process.Command)
		if len(process.Ports) > 0 {
			ports := make([]string, len(process.Ports))
			for i, port := range process.Ports {
				ports[i] = fmt.Sprintf("%d", port)
			}
			label += " (listening ports " + strings.Join(ports, ",") + ")"
		}
		labels = append(labels, label)
	}
	summary := fmt.Sprintf("%s teardown blocked by residual processes: %s", taskDisplayName(task), strings.Join(labels, "; "))
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		return err
	}
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == "teardown_blocked" && notice.AckedAt == 0 && notice.Summary == summary {
			return nil
		}
	}
	data, err := json.Marshal(map[string]any{"processes": processes, "error": cause.Error()})
	if err != nil {
		return err
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "teardown_blocked", Summary: summary, DataJSON: string(data)}); err != nil {
		return err
	}
	return nil
}

func mergeTaskProcessDetails(groups ...[]taskProcessDetail) []taskProcessDetail {
	byPID := map[int]taskProcessDetail{}
	for _, group := range groups {
		for _, process := range group {
			if process.PID < 2 {
				continue
			}
			if old, found := byPID[process.PID]; found {
				if process.Command == "" {
					process.Command = old.Command
				}
				if len(process.Ports) == 0 {
					process.Ports = old.Ports
				}
			}
			byPID[process.PID] = process
		}
	}
	result := make([]taskProcessDetail, 0, len(byPID))
	for _, process := range byPID {
		result = append(result, process)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PID < result[j].PID })
	return result
}
