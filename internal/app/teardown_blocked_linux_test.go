//go:build linux

package app

import (
	"os"
	"testing"
	"time"
)

func TestStableTaskProcessesDropsProcessesThatDoNotSurviveRescan(t *testing.T) {
	first := []taskProcessDetail{{PID: 10}, {PID: 20}, {PID: 30}}
	got := stableTaskProcesses(first, func() []taskProcessDetail { return []taskProcessDetail{{PID: 10}, {PID: 30}, {PID: 40}} })
	if len(got) != 2 || got[0].PID != 10 || got[1].PID != 30 {
		t.Fatalf("stable processes = %#v, want PIDs 10 and 30", got)
	}
	if got := stableTaskProcesses(nil, func() []taskProcessDetail { t.Fatal("rescanned an empty set"); return nil }); got != nil {
		t.Fatalf("empty set = %#v", got)
	}
}

func TestSandboxProcessStartedBefore(t *testing.T) {
	pid := os.Getpid()
	if !sandboxProcessStartedBefore(pid, time.Now().Add(time.Hour)) {
		t.Fatal("running test process should predate a future instant")
	}
	if sandboxProcessStartedBefore(pid, time.Now().Add(-24*time.Hour)) {
		t.Fatal("running test process should not predate yesterday")
	}
	if sandboxProcessStartedBefore(1<<30, time.Now()) {
		t.Fatal("missing process must be treated as new")
	}
}
