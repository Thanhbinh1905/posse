package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) recordTeardownBlocked(ctx context.Context, db *store.DB, project store.Project, task store.Task, cause error) error {
	collect := func() []taskProcessDetail {
		processes := taskContainmentProcessDetails(task)
		if task.WorktreePath != "" {
			sandboxProcesses, err := sandboxTaskProcessDetails(task.WorktreePath)
			if err == nil {
				processes = mergeTaskProcessDetails(processes, sandboxProcesses)
			}
		}
		if home, err := s.homePath(); err == nil {
			if roots, rootErr := taskOwnedProcessRoots(ctx, db, home, project, task); rootErr == nil {
				if pids, pidErr := processIDsInRoots(roots); pidErr == nil {
					processes = mergeTaskProcessDetails(processes, taskOwnedProcessDetails(pids))
				}
			}
		}
		return processes
	}
	processes := stableTaskProcesses(collect(), collect)
	if len(processes) == 0 {
		return nil
	}
	remedy := "Stop the listed processes or move their cwd and open files outside the Sandbox, then retry Teardown."
	summary := fmt.Sprintf("%s teardown blocked by residual processes: %s. Remedy: %s", taskDisplayName(task), describeTaskProcesses(processes), remedy)
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		return err
	}
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == "teardown_blocked" && notice.AckedAt == 0 && notice.Summary == summary {
			return nil
		}
	}
	data, err := json.Marshal(map[string]any{"processes": processes, "error": cause.Error(), "remedy": remedy})
	if err != nil {
		return err
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "teardown_blocked", Summary: summary, DataJSON: string(data)}); err != nil {
		return err
	}
	return nil
}

// transientProcessWindow separates the two scans behind stableTaskProcesses.
// It exceeds the lifetime of the `sleep 0.1` a polling loop spawns.
const transientProcessWindow = 200 * time.Millisecond

// stableTaskProcesses keeps the processes that survive a second scan. Short-lived
// children, such as the `sleep` a polling shell loop spawns each iteration,
// get a new PID on every scan and would otherwise make the blocking set differ
// between retries.
func stableTaskProcesses(first []taskProcessDetail, rescan func() []taskProcessDetail) []taskProcessDetail {
	if len(first) == 0 {
		return nil
	}
	time.Sleep(transientProcessWindow)
	still := make(map[int]bool)
	for _, process := range rescan() {
		still[process.PID] = true
	}
	stable := make([]taskProcessDetail, 0, len(first))
	for _, process := range first {
		if still[process.PID] {
			stable = append(stable, process)
		}
	}
	return stable
}

func describeTaskProcesses(processes []taskProcessDetail) string {
	labels := make([]string, 0, len(processes))
	for _, process := range processes {
		command := strings.TrimSpace(process.Command)
		if command == "" {
			command = "unknown command"
		}
		label := fmt.Sprintf("PID %d %s", process.PID, command)
		if len(process.Ports) > 0 {
			ports := make([]string, len(process.Ports))
			for index, port := range process.Ports {
				ports[index] = fmt.Sprintf("%d", port)
			}
			label += " (listening ports " + strings.Join(ports, ",") + ")"
		}
		labels = append(labels, label)
	}
	return strings.Join(labels, "; ")
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

// recordTeardownEscapedProcesses reports Task-marked processes that outlived
// Teardown because they no longer reference the Sandbox or Task scratch.
func recordTeardownEscapedProcesses(ctx context.Context, db *store.DB, project store.Project, task store.Task, processes []taskProcessDetail) error {
	if len(processes) == 0 {
		return nil
	}
	remedy := "Teardown could not prove these processes belong to the Task, so it left them running. Stop them if they are leftovers of the Task."
	summary := fmt.Sprintf("%s was torn down, but Task-marked processes outside its Sandbox survived: %s. %s", taskDisplayName(task), describeTaskProcesses(processes), remedy)
	data, err := json.Marshal(map[string]any{"processes": processes, "remedy": remedy})
	if err != nil {
		return err
	}
	_, err = db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "teardown_escaped_processes", Summary: summary, DataJSON: string(data)})
	return err
}
