package runtime

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

// The test process stands in for the agent: herdr.Fake reports it as the
// pane's foreground process.
func startChild(t *testing.T, ownGroup bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: ownGroup}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestBackgroundJobSuppressesWorkerIdleUntilItEnds(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "idle"}}}
	progress := &fakeProgress{output: "turn ended", worktree: "unchanged"}
	job := startChild(t, true)

	for _, offset := range []time.Duration{0, time.Minute, 5 * time.Minute} {
		result, err := Run(ctx, db, fake, project.ID, 0, time.Minute, start.Add(offset), progress)
		if err != nil || len(result.Notices) != 0 {
			t.Fatalf("idle Worker with a running background job at +%s: %#v, %v", offset, result.Notices, err)
		}
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil || updated.IdleSince != 0 {
		t.Fatalf("idle clock ran during the background job: %#v, %v", updated, err)
	}

	_ = job.Process.Kill()
	_ = job.Wait()
	if _, err := Run(ctx, db, fake, project.ID, 0, time.Minute, start.Add(6*time.Minute), progress); err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, db, fake, project.ID, 0, time.Minute, start.Add(7*time.Minute), progress)
	if err != nil || len(result.Notices) != 1 || result.Notices[0].Kind != "worker_idle" {
		t.Fatalf("stalled Worker after the job ended: %#v, %v", result.Notices, err)
	}
}

func TestChildInAgentGroupIsNotABackgroundJob(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "idle"}}}
	progress := &fakeProgress{output: "turn ended", worktree: "unchanged"}
	startChild(t, false)

	if _, err := Run(ctx, db, fake, project.ID, 0, time.Minute, start, progress); err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, db, fake, project.ID, 0, time.Minute, start.Add(time.Minute), progress)
	if err != nil || len(result.Notices) != 1 || result.Notices[0].Kind != "worker_idle" {
		t.Fatalf("MCP-style child suppressed worker_idle: %#v, %v", result.Notices, err)
	}
}

func TestProcessTableGroupLeaderBelow(t *testing.T) {
	table := processTable{
		10: {parent: 1, group: 10},  // agent
		11: {parent: 10, group: 10}, // MCP server
		12: {parent: 11, group: 10}, // MCP helper
		20: {parent: 10, group: 20}, // Bash tool shell
		21: {parent: 20, group: 20},
		30: {parent: 1, group: 30}, // unrelated leader
	}
	if !table.hasGroupLeaderBelow(10) {
		t.Fatal("Bash tool shell below the agent not found")
	}
	if table.hasGroupLeaderBelow(11) || table.hasGroupLeaderBelow(30) {
		t.Fatal("a leader outside the agent's subtree was counted")
	}
}
