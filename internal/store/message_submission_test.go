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

func TestMessageClaimIsExclusivePerTask(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "scout", Title: "Investigate", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	firstID, err := db.QueueMessage(ctx, taskID, "first", false)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := db.QueueMessage(ctx, taskID, "second", false)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimMessage(ctx, firstID, "first-token", time.Now().UnixMilli())
	if err != nil || !claimed {
		t.Fatalf("first message claim=%t err=%v", claimed, err)
	}
	claimed, err = db.ClaimMessage(ctx, secondID, "second-token", time.Now().UnixMilli())
	if err != nil || claimed {
		t.Fatalf("second message claim=%t err=%v, want exclusive claim", claimed, err)
	}
}

func TestRejectedInterruptRedirectStaysAheadOfQueuedMessages(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "scout", Title: "Investigate", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	queuedID, err := db.QueueMessage(ctx, taskID, "older queue item", true)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixMilli()
	redirectID, err := db.QueueInterruptMessage(ctx, taskID, "urgent redirect", "interrupt-token", at)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMessageSubmitting(ctx, redirectID, "interrupt-token"); err != nil {
		t.Fatal(err)
	}
	if err := db.RollbackInterruptMessage(ctx, redirectID, "interrupt-token"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseMessageClaims(ctx, taskID, "interrupt-token", 0); err != nil {
		t.Fatal(err)
	}
	oldest, err := db.OldestQueuedMessage(ctx, taskID)
	if err != nil || oldest.ID != redirectID {
		t.Fatalf("oldest queued message after rejected redirect = %#v, %v", oldest, err)
	}
	queued, err := db.MessageByID(ctx, queuedID)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("older message after redirect rollback = %#v, %v", queued, err)
	}
}

func TestInterruptReservationExpiresWithoutReplayingRedirect(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "scout", Title: "Investigate", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	queuedID, err := db.QueueMessage(ctx, taskID, "wait for idle", true)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UnixMilli()
	redirectID, err := db.QueueInterruptMessage(ctx, taskID, "stop and redirect", "interrupt-token", at)
	if err != nil {
		t.Fatal(err)
	}
	active, err := db.HasDeliveryInProgress(ctx, taskID)
	if err != nil || !active {
		t.Fatalf("interrupt reservation active=%t err=%v", active, err)
	}
	if _, err := db.QueueInterruptMessage(ctx, taskID, "second redirect", "other-token", at); err != ErrMessageDeliveryInProgress {
		t.Fatalf("competing interrupt error=%v, want ErrMessageDeliveryInProgress", err)
	}
	if err := db.ReleaseExpiredDeliveryClaims(ctx, at+3*60*1000); err != nil {
		t.Fatal(err)
	}
	queued, err := db.MessageByID(ctx, queuedID)
	if err != nil || queued.Status != "queued" {
		t.Fatalf("reserved older message after expiry = %#v, %v", queued, err)
	}
	redirect, err := db.MessageByID(ctx, redirectID)
	if err != nil || redirect.Status != "withdrawn" {
		t.Fatalf("unverified redirect after expiry = %#v, %v", redirect, err)
	}
	active, err = db.HasDeliveryInProgress(ctx, taskID)
	if err != nil || active {
		t.Fatalf("expired interrupt remains active=%t err=%v", active, err)
	}
}
