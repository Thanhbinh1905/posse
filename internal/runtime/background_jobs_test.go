package runtime

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestClaudeBackgroundChildDoesNotDelayWorkerIdleNotice(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "idle"}}}
	progress := &fakeProgress{output: "turn ended", worktree: "unchanged"}
	child := exec.Command("sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.Process != nil {
			if child.Process.Pid > 1 {
				_ = child.Process.Kill()
			}
			_ = child.Wait()
		}
	})

	for _, offset := range []time.Duration{0, time.Minute - time.Millisecond} {
		result, err := Run(ctx, db, fake, project.ID, 0, time.Minute, start.Add(offset), progress)
		if err != nil || len(result.Notices) != 0 {
			t.Fatalf("idle Claude Worker with a background child at +%s: %#v, %v", offset, result.Notices, err)
		}
	}
	result, err := Run(ctx, db, fake, project.ID, 0, time.Minute, start.Add(time.Minute), progress)
	if err != nil || len(result.Notices) != 1 || result.Notices[0].Kind != "worker_idle" || result.Notices[0].TaskID != task.ID || result.Notices[0].Summary != task.Title+" is idle without a Signal" {
		t.Fatalf("idle Claude Worker with a background child did not receive a Notice after idle_after: %#v, %v", result.Notices, err)
	}
}
