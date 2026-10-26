//go:build linux

package app

import (
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

func TestTaskDiskLeakSummarySkipsSandboxReusedByLiveTask(t *testing.T) {
	home := t.TempDir()
	sandbox := t.TempDir()
	project := store.Project{Name: "shop"}
	torn := store.Task{ID: 1, Seq: 1, State: store.StateTornDown, WorktreePath: sandbox}
	live := store.Task{ID: 2, Seq: 2, State: store.StateWorking, WorktreePath: sandbox}
	pid := startSleepIn(t, sandbox)

	status, detail := taskDiskLeakSummary(home, project, []store.Task{torn, live})
	if status != "ok" || strings.Contains(detail, "residual processes") {
		t.Fatalf("live Task process reported as a leak of t1: %s %s", status, detail)
	}

	status, detail = taskDiskLeakSummary(home, project, []store.Task{torn})
	if status != "warn" || !strings.Contains(detail, "t1 PID") {
		t.Fatalf("real leak of t1 (PID %d) not reported: %s %s", pid, status, detail)
	}
}
