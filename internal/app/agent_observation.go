package app

import (
	"fmt"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// agentObservation labels what Herdr last reported about a live Task's agent,
// such as "idle 7h12m" or "no pane". It sits beside the Task state, which stays
// authoritative: Herdr done means the agent ended a turn, not that the Task is
// done. mismatch is set when a working Task's agent has been idle without a
// Signal for idle_after, the same clock that raises worker_idle.
func agentObservation(task store.Task, cfg config.Config, now time.Time) (label, mismatch string) {
	if !store.ObservedState(task.State) || task.AgentStatus == "" {
		return "", ""
	}
	if task.AgentStatus == store.AgentStatusNoPane {
		return "no pane", ""
	}
	since := task.AgentStatusSince
	turnEnded := task.AgentStatus == "idle" || task.AgentStatus == "done"
	if turnEnded && task.IdleSince > 0 {
		since = task.IdleSince
	}
	status := task.AgentStatus
	if status == store.AgentStatusNoAgent {
		status = "no agent"
	}
	label = status
	if since > 0 {
		label += " " + compactAge(now.Sub(time.UnixMilli(since)))
	}
	idleAfter := duration(cfg.Defaults.IdleAfter)
	if task.State == store.StateWorking && turnEnded && task.IdleSince > 0 && idleAfter > 0 && now.Sub(time.UnixMilli(task.IdleSince)) >= idleAfter {
		mismatch = "Task working, but agent idle without a Signal past idle_after " + cfg.Defaults.IdleAfter
	}
	return label, mismatch
}

// compactAge renders an age as 45s, 12m or 7h12m.
func compactAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", int(age/time.Second))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	default:
		return fmt.Sprintf("%dh%dm", int(age/time.Hour), int(age%time.Hour/time.Minute))
	}
}
