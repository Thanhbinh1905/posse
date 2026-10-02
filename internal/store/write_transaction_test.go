package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeadlineBoundedWritesRestoreBusyTimeout(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "bounded", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "scout", Title: "Bounded write", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for _, testCase := range []struct {
		name  string
		write func(context.Context) error
	}{
		{"transaction", func(ctx context.Context) error {
			return db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started")
		}},
		{"generated exec", func(ctx context.Context) error {
			return db.UpdateProgress(ctx, taskID, "output", "worktree", time.Now().UnixMilli())
		}},
		{"direct exec", func(ctx context.Context) error {
			return db.CreateWorkerExitedNotice(ctx, Task{ID: taskID, ProjectID: project.ID, Title: "Bounded"}, time.Now())
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			writeCtx, cancel := context.WithTimeout(ctx, 75*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := testCase.write(writeCtx)
			if !IsBusy(err) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("write returned %v, want typed contention", err)
			}
			if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
				t.Fatalf("write outlived deadline: %s", elapsed)
			}
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var timeout int
			if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
				t.Fatal(err)
			}
			if timeout != sqliteBusyTimeoutMillis {
				t.Fatalf("pooled busy timeout = %d, want %d", timeout, sqliteBusyTimeoutMillis)
			}
		})
	}
}
