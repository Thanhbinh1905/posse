//go:build linux

package app

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func startSleepIn(t *testing.T, dir string) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestTaskDiskLeakSummaryAttributesSandboxProcessesOnlyToItsHolder(t *testing.T) {
	home := t.TempDir()
	project := store.Project{Name: "shop"}
	reused := t.TempDir()
	idle := t.TempDir()
	held := t.TempDir()
	// A pane shell of a live Task in a Sandbox that an older Task used.
	startSleepIn(t, reused)
	// A shell in a released Sandbox that many finished Tasks used.
	startSleepIn(t, idle)
	tasks := []store.Task{
		{ID: 1, Seq: 1, State: store.StateTornDown, WorktreePath: reused},
		{ID: 2, Seq: 2, State: store.StateDone, WorktreePath: reused, SandboxID: 7},
		{ID: 3, Seq: 3, State: store.StateTornDown, WorktreePath: idle},
		{ID: 4, Seq: 4, State: store.StateTornDown, WorktreePath: idle},
		{ID: 5, Seq: 5, State: store.StateMerged, WorktreePath: held, SandboxID: 9},
	}
	status, detail := taskDiskLeakSummary(home, project, tasks)
	if status != "ok" || strings.Contains(detail, "residual processes") {
		t.Fatalf("processes outside a held Task Sandbox were attributed to finished Tasks: %s %s", status, detail)
	}

	pid := startSleepIn(t, held)
	status, detail = taskDiskLeakSummary(home, project, tasks)
	if status != "warn" || !strings.Contains(detail, fmt.Sprintf("t5 PID %d", pid)) || strings.Contains(detail, "t1 PID") || strings.Contains(detail, "t3 PID") || strings.Contains(detail, "t4 PID") {
		t.Fatalf("leak in the Sandbox t5 still holds (PID %d) not attributed to t5 alone: %s %s", pid, status, detail)
	}
}
