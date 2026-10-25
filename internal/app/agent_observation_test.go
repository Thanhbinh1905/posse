package app

import (
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestAgentObservationLabelsStatusAgeAndIdleMismatch(t *testing.T) {
	now := time.UnixMilli(10 * int64(time.Hour/time.Millisecond))
	ago := func(age time.Duration) int64 { return now.Add(-age).UnixMilli() }
	var cfg config.Config
	cfg.Defaults.IdleAfter = "3m"
	for _, test := range []struct {
		name          string
		task          store.Task
		label, differ string
	}{
		{"not observed yet", store.Task{State: store.StateWorking}, "", ""},
		{"queued Task", store.Task{State: store.StateQueued, AgentStatus: "working", AgentStatusSince: ago(time.Minute)}, "", ""},
		{"working", store.Task{State: store.StateWorking, AgentStatus: "working", AgentStatusSince: ago(45 * time.Second)}, "working 45s", ""},
		{"idle uses the worker_idle clock", store.Task{State: store.StateWorking, AgentStatus: "idle", AgentStatusSince: ago(time.Minute), IdleSince: ago(7*time.Hour + 12*time.Minute)}, "idle 7h12m", "Task working, but agent idle without a Signal past idle_after 3m"},
		{"idle below idle_after", store.Task{State: store.StateWorking, AgentStatus: "done", AgentStatusSince: ago(2 * time.Minute), IdleSince: ago(2 * time.Minute)}, "done 2m", ""},
		{"idle with a background job", store.Task{State: store.StateWorking, AgentStatus: "idle", AgentStatusSince: ago(time.Hour)}, "idle 1h0m", ""},
		{"done Task whose agent ended its turn", store.Task{State: store.StateDone, AgentStatus: "done", AgentStatusSince: ago(time.Hour)}, "done 1h0m", ""},
		{"no pane", store.Task{State: store.StateWorking, AgentStatus: store.AgentStatusNoPane, AgentStatusSince: ago(time.Minute)}, "no pane", ""},
		{"no agent", store.Task{State: store.StateWorking, AgentStatus: store.AgentStatusNoAgent, AgentStatusSince: ago(time.Minute)}, "no agent 1m", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			label, mismatch := agentObservation(test.task, cfg, now)
			if label != test.label || mismatch != test.differ {
				t.Fatalf("agentObservation = %q, %q; want %q, %q", label, mismatch, test.label, test.differ)
			}
		})
	}
}
