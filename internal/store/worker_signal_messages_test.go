package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestDoneSignalRefusesUndeliveredTaskInstructions(t *testing.T) {
	for _, status := range []string{"queued", "claimed", "interrupting"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			project, err := db.CreateProject(ctx, "shop", "/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "scout", Title: "Investigate", LandingMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
				t.Fatal(err)
			}
			messageID, err := db.QueueMessage(ctx, taskID, "Apply this instruction", true)
			if err != nil {
				t.Fatal(err)
			}
			if status != "queued" {
				if _, err := db.ExecContext(ctx, `UPDATE messages SET status=? WHERE id=?`, status, messageID); err != nil {
					t.Fatal(err)
				}
			}
			task, err := db.Task(ctx, project.ID, "t1")
			if err != nil {
				t.Fatal(err)
			}
			state, err := db.RecordWorkerSignal(ctx, task, "done", "Report ready", nil, "task_done")
			if state != "" || !errors.Is(err, ErrQueuedInstructionsPending) {
				t.Fatalf("done Signal result = %q, %v; want undelivered-instruction refusal", state, err)
			}

			updatedTask, err := db.Task(ctx, project.ID, "t1")
			if err != nil || updatedTask.State != StateWorking {
				t.Fatalf("refused done Signal changed Task state: %#v, %v", updatedTask, err)
			}
			message, err := db.MessageByID(ctx, messageID)
			if err != nil || message.Status != status {
				t.Fatalf("refused done Signal changed message: %#v, %v", message, err)
			}
			var signals, notices int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM signals WHERE task_id=?`, taskID).Scan(&signals); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=?`, taskID).Scan(&notices); err != nil {
				t.Fatal(err)
			}
			if signals != 0 || notices != 0 {
				t.Fatalf("refused done Signal left records: signals=%d notices=%d", signals, notices)
			}
		})
	}
}
