package store

import (
	"context"
	"testing"
	"time"
)

func TestMessageSubmissionAndTaskReopenAreAtomic(t *testing.T) {
	for _, failTransition := range []bool{false, true} {
		name := "submitted"
		if failTransition {
			name = "transition failure rolls back submission"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
			if err != nil {
				t.Fatal(err)
			}
			id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			if err := db.SetTaskGatedSHA(ctx, id, "previous-head"); err != nil {
				t.Fatal(err)
			}
			messageID, err := db.QueueMessage(ctx, id, "Fix the failure", false)
			if err != nil {
				t.Fatal(err)
			}
			if claimed, err := db.ClaimMessage(ctx, messageID, "claim", time.Now().UnixMilli()); err != nil || !claimed {
				t.Fatalf("claim=%t err=%v", claimed, err)
			}
			if failTransition {
				if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_reopen BEFORE UPDATE OF state ON tasks BEGIN SELECT RAISE(ABORT,'reject reopen'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = db.MarkMessageSubmitting(ctx, messageID, "claim")
			if (err != nil) != failTransition {
				t.Fatalf("submission error=%v, want failure=%t", err, failTransition)
			}
			task, err := db.TaskByID(ctx, project.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			message, err := db.MessageByID(ctx, messageID)
			if err != nil {
				t.Fatal(err)
			}
			if failTransition {
				if task.State != StateDone || task.GatedSHA != "previous-head" || message.Status != "claimed" {
					t.Fatalf("failed submission partially committed: task=%#v message=%#v", task, message)
				}
			} else if task.State != StateWorking || task.GatedSHA != "" || message.Status != "submitting" {
				t.Fatalf("submission did not authorize immediate Signal: task=%#v message=%#v", task, message)
			}
		})
	}
}
