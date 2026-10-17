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
			if claimed, err := db.ClaimMessage(ctx, messageID, "claim", time.Now().UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli()); err != nil || !claimed {
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

func TestQueueMessageOnceAfterCompletionIsIdempotentAndAllowsLaterRetry(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateWorking, StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	body := "Merge the current target"
	id, queued, err := db.QueueMessageOnceAfter(ctx, taskID, body, true, 0)
	if err != nil || !queued || id == 0 {
		t.Fatalf("first queue result=(%d,%t,%v)", id, queued, err)
	}
	if id, queued, err := db.QueueMessageOnceAfter(ctx, taskID, body, true, 0); err != nil || queued || id != 0 {
		t.Fatalf("duplicate queue result=(%d,%t,%v)", id, queued, err)
	}
	if id, queued, err := db.QueueMessageOnceAfter(ctx, taskID, body, true, time.Now().Add(time.Second).UnixMilli()); err != nil || !queued || id == 0 {
		t.Fatalf("new completion queue result=(%d,%t,%v)", id, queued, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND body=?`, taskID, body).Scan(&count); err != nil || count != 2 {
		t.Fatalf("queued messages=%d err=%v, want one per completion", count, err)
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
	claimed, err := db.ClaimMessage(ctx, firstID, "first-token", time.Now().UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli())
	if err != nil || !claimed {
		t.Fatalf("first message claim=%t err=%v", claimed, err)
	}
	claimed, err = db.ClaimMessage(ctx, secondID, "second-token", time.Now().UnixMilli(), time.Now().Add(-2*time.Minute).UnixMilli())
	if err != nil || claimed {
		t.Fatalf("second message claim=%t err=%v, want exclusive claim", claimed, err)
	}
}

func TestConcurrentMessageClaimsRemainExclusive(t *testing.T) {
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
	at := time.Now().UnixMilli()
	activeBefore := at - 2*time.Minute.Milliseconds()
	start := make(chan struct{})
	type claimResult struct {
		claimed bool
		err     error
	}
	results := make(chan claimResult, 2)
	for index, messageID := range []int64{firstID, secondID} {
		go func(messageID int64, token string) {
			<-start
			claimed, err := db.ClaimMessage(ctx, messageID, token, at, activeBefore)
			results <- claimResult{claimed: claimed, err: err}
		}(messageID, []string{"first-token", "second-token"}[index])
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.claimed == second.claimed {
		t.Fatalf("concurrent claims were not exclusive: first=%#v second=%#v", first, second)
	}
}

func TestExpiredUncertainSubmissionAllowsFreshClaimsWithoutReplayingOriginal(t *testing.T) {
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
	at := time.Now().UnixMilli()
	activeBefore := at - 2*time.Minute.Milliseconds()
	originalID, err := db.QueueMessage(ctx, taskID, "ambiguous original", false)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimMessage(ctx, originalID, "uncertain-token", at-3*time.Minute.Milliseconds(), at-5*time.Minute.Milliseconds()); err != nil || !claimed {
		t.Fatalf("claim original=%t err=%v", claimed, err)
	}
	if err := db.MarkMessageSubmitting(ctx, originalID, "uncertain-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateMessageDeliveryUncertainNotice(ctx, project.ID, taskID, originalID, "original delivery uncertain", "ambiguous original"); err != nil {
		t.Fatal(err)
	}
	freshID, err := db.QueueMessage(ctx, taskID, "fresh replacement", false)
	if err != nil {
		t.Fatal(err)
	}
	active, err := db.HasDeliveryInProgress(ctx, taskID, activeBefore)
	if err != nil || active {
		t.Fatalf("expired uncertain row still owns delivery lock: active=%t err=%v", active, err)
	}
	if claimed, err := db.ClaimMessage(ctx, freshID, "fresh-token", at, activeBefore); err != nil || !claimed {
		t.Fatalf("fresh message claim=%t err=%v", claimed, err)
	}
	if err := db.ReleaseMessageClaims(ctx, taskID, "fresh-token", 0); err != nil {
		t.Fatal(err)
	}
	redirectID, err := db.QueueInterruptMessage(ctx, taskID, "fresh stop and redirect", "fresh-interrupt", at, activeBefore)
	if err != nil {
		t.Fatalf("fresh interrupt was blocked by expired uncertainty: %v", err)
	}
	original, err := db.MessageByID(ctx, originalID)
	if err != nil || original.Status != "submitting" {
		t.Fatalf("uncertain original was rewritten: %#v err=%v", original, err)
	}
	redirect, err := db.MessageByID(ctx, redirectID)
	if err != nil || redirect.Status != "interrupting" {
		t.Fatalf("fresh redirect=%#v err=%v", redirect, err)
	}
	var notices int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='message_delivery_uncertain'`, taskID).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("uncertainty Notice count=%d err=%v", notices, err)
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
	redirectID, err := db.QueueInterruptMessage(ctx, taskID, "urgent redirect", "interrupt-token", at, at-2*time.Minute.Milliseconds())
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
	redirectID, err := db.QueueInterruptMessage(ctx, taskID, "stop and redirect", "interrupt-token", at, at-2*time.Minute.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	active, err := db.HasDeliveryInProgress(ctx, taskID, at-2*time.Minute.Milliseconds())
	if err != nil || !active {
		t.Fatalf("interrupt reservation active=%t err=%v", active, err)
	}
	if _, err := db.QueueInterruptMessage(ctx, taskID, "second redirect", "other-token", at, at-2*time.Minute.Milliseconds()); err != ErrMessageDeliveryInProgress {
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
	active, err = db.HasDeliveryInProgress(ctx, taskID, at-2*time.Minute.Milliseconds())
	if err != nil || active {
		t.Fatalf("expired interrupt remains active=%t err=%v", active, err)
	}
}
