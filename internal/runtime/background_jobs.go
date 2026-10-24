package runtime

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

// Claude Code reports `idle` to Herdr when a turn ends, even while a
// background shell it started keeps running and will wake it on completion.
// Herdr's snapshot has no field for that, so the Worker's own process tree is
// the signal: Claude runs each Bash tool call in a child that leads its own
// process group, while MCP servers and hooks stay in the agent's group. At an
// idle turn boundary any such child is a background job.

type backgroundJobsKey struct{}

// processTable lists live processes as pid -> {parent, process group}.
type processTable map[int]processEntry

type processEntry struct{ parent, group int }

type backgroundJobDetector struct {
	adapter herdr.Adapter
	list    func(context.Context) (processTable, error)
	once    sync.Once
	table   processTable
	err     error
}

func withBackgroundJobDetector(ctx context.Context, adapter herdr.Adapter) context.Context {
	return context.WithValue(ctx, backgroundJobsKey{}, &backgroundJobDetector{adapter: adapter, list: listProcesses})
}

// paneHasBackgroundJob reports whether a Claude Code pane's agent still owns a
// background job. Any failure to find out reports false, which keeps the
// previous behavior of treating the idle Worker as idle.
func paneHasBackgroundJob(ctx context.Context, pane herdr.Pane) bool {
	detector, _ := ctx.Value(backgroundJobsKey{}).(*backgroundJobDetector)
	if detector == nil || pane.Agent != "claude" {
		return false
	}
	raw, err := detector.adapter.Call(ctx, "pane.process_info", map[string]any{"pane_id": pane.PaneID})
	if err != nil {
		return false
	}
	var info struct {
		ProcessInfo struct {
			ForegroundGroup int `json:"foreground_process_group_id"`
		} `json:"process_info"`
	}
	if json.Unmarshal(raw, &info) != nil {
		return false
	}
	agent := info.ProcessInfo.ForegroundGroup
	if agent <= 1 {
		return false
	}
	detector.once.Do(func() { detector.table, detector.err = detector.list(ctx) })
	return detector.err == nil && detector.table.hasGroupLeaderBelow(agent)
}

// hasGroupLeaderBelow reports whether a descendant of root leads its own
// process group.
func (table processTable) hasGroupLeaderBelow(root int) bool {
	for pid, entry := range table {
		if pid == root || entry.group != pid {
			continue
		}
		for ancestor, depth := entry.parent, 0; ancestor > 1 && depth < len(table); depth++ {
			if ancestor == root {
				return true
			}
			parent, ok := table[ancestor]
			if !ok {
				break
			}
			ancestor = parent.parent
		}
	}
	return false
}

// listProcesses uses the ps flags common to procps and BSD ps.
func listProcesses(ctx context.Context) (processTable, error) {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return nil, err
	}
	table := processTable{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		parent, err2 := strconv.Atoi(fields[1])
		group, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		table[pid] = processEntry{parent: parent, group: group}
	}
	return table, nil
}
