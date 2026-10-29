//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
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

func TestTeardownBlockedNoticeIgnoresProcessesInAReleasedSandbox(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(root, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	released := t.TempDir()
	held := t.TempDir()
	// The pane shell of another Task now working in the Sandbox this Task released.
	startSleepIn(t, released)
	addTask := func(seq int, path, mountState string) store.Task {
		t.Helper()
		taskID, err := db.CreateTask(ctx, project.ID, store.Task{Seq: seq, Type: "change", Title: "Work", State: store.StateSpawning, MergeMode: "pr", WorktreePath: path})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET state=? WHERE id=?`, store.StateMerged, taskID); err != nil {
			t.Fatal(err)
		}
		var owner any
		if mountState == "held" {
			owner = taskID
		}
		result, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id,acquired_at,released_at) VALUES(?,?,?,?,?,0,0)`, project.ID, seq, path, mountState, owner)
		if err != nil {
			t.Fatal(err)
		}
		if mountState == "held" {
			sandboxID, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateTaskSandboxPath(ctx, taskID, sandboxID, path); err != nil {
				t.Fatal(err)
			}
		}
		task, err := db.TaskByID(ctx, project.ID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	finished := addTask(1, released, "idle")
	holder := addTask(2, held, "held")
	service := testService(home, nil)
	cause := errors.New("branch removal failed")
	if err := service.recordTeardownBlocked(ctx, db, project, finished, cause); err != nil {
		t.Fatal(err)
	}
	pid := startSleepIn(t, held)
	if err := service.recordTeardownBlocked(ctx, db, project, holder, cause); err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(ctx, project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	blocked := map[int64]string{}
	for _, notice := range notices {
		if notice.Kind == "teardown_blocked" {
			blocked[notice.TaskID] = notice.Summary
		}
	}
	if summary, found := blocked[finished.ID]; found {
		t.Fatalf("a process in the released Sandbox was attributed to t1: %s", summary)
	}
	if !strings.Contains(blocked[holder.ID], fmt.Sprintf("PID %d", pid)) {
		t.Fatalf("a process in the Sandbox t2 holds (PID %d) was not reported: %#v", pid, blocked)
	}
}
