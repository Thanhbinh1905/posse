package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/dispatch"
)

func TestTaskDependencyReadinessRequiresSealedReportAndExplicitAcceptance(t *testing.T) {
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
	reportID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Ticket Report", ReportKind: "tickets"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='reported',report_digest='sealed-digest' WHERE id=?`, reportID); err != nil {
		t.Fatal(err)
	}
	accepted := []dispatch.Dependency{{TaskID: "t1", Milestone: "accepted"}}
	unmet, err := db.UnmetTaskDependencies(ctx, project.ID, accepted)
	if err != nil || len(unmet) != 1 {
		t.Fatalf("unaccepted Report prerequisite was treated as ready: unmet=%v err=%v", unmet, err)
	}
	reported := []dispatch.Dependency{{TaskID: "t1", Milestone: "reported"}}
	unmet, err = db.UnmetTaskDependencies(ctx, project.ID, reported)
	if err != nil || len(unmet) != 0 {
		t.Fatalf("sealed reported prerequisite remained unmet: unmet=%v err=%v", unmet, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO report_acceptances(report_task_id,report_digest,accepted_by,recorded_by,provenance,user_quote,accepted_at) VALUES(?,'sealed-digest','user','user','user-command','',1)`, reportID); err != nil {
		t.Fatal(err)
	}
	unmet, err = db.UnmetTaskDependencies(ctx, project.ID, accepted)
	if err != nil || len(unmet) != 0 {
		t.Fatalf("explicitly accepted Report prerequisite remained unmet: unmet=%v err=%v", unmet, err)
	}
}

func TestQueuedTaskClaimIsAtomicCapacityBoundAndSingleUse(t *testing.T) {
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
	activeID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Active", ShortName: "active"}, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	queuedID, queuedSeq, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Queued", ShortName: "queued", State: StateQueued}, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimQueuedTask(ctx, project.ID, queuedID, 1, TaskBranchName(queuedSeq), os.Getpid())
	if err != nil || claimed {
		t.Fatalf("queued Task claimed while capacity was full: claimed=%v err=%v", claimed, err)
	}
	if err := db.Transition(ctx, activeID, StateSpawning, StateFailed, "cli", "fixture Worker failed"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claimResults := make(chan struct {
		claimed bool
		err     error
	}, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := db.ClaimQueuedTask(ctx, project.ID, queuedID, 1, TaskBranchName(queuedSeq), os.Getpid())
			claimResults <- struct {
				claimed bool
				err     error
			}{claimed: claimed, err: err}
		}()
	}
	wg.Wait()
	close(claimResults)
	claims := 0
	for result := range claimResults {
		if result.err != nil {
			t.Fatalf("concurrent queued claim: %v", result.err)
		}
		if result.claimed {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("concurrent claim winners=%d, want exactly one", claims)
	}
	task, err := db.TaskByID(ctx, project.ID, queuedID)
	if err != nil || task.State != StateSpawning || task.Branch != TaskBranchName(queuedSeq) {
		t.Fatalf("claim did not atomically set Task state and branch: task=%+v err=%v", task, err)
	}
	intent, err := db.IntentByTask(ctx, queuedID)
	if err != nil || intent.Command != "start" {
		t.Fatalf("claim did not create normal start intent: intent=%+v err=%v", intent, err)
	}
	claimed, err = db.ClaimQueuedTask(ctx, project.ID, queuedID, 1, TaskBranchName(queuedSeq), os.Getpid())
	if err != nil || claimed {
		t.Fatalf("claim was not single-use: claimed=%v err=%v", claimed, err)
	}
	if _, _, err := db.CreateTaskWithSequenceAndIntentWithinLimit(ctx, project.ID, project.Name, Task{Type: "change", Title: "Over limit", ShortName: "over-limit"}, "start", os.Getpid(), 1, func(int) (bool, error) { return false, nil }); !errors.Is(err, ErrWorkerLimit) {
		t.Fatalf("new spawn admission ignored the reserved slot: %v", err)
	}
}

func TestQueuedTaskIsDurableAndResourceFree(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, seq, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "research", Title: "Queued research", ShortName: "queued-research", State: StateQueued}, func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Seq != seq || task.State != StateQueued || task.Branch != "" || task.WorktreePath != "" || task.SandboxID != 0 || task.PaneID != "" {
		t.Fatalf("queued Task is not durable and resource-free: %+v", task)
	}
	count, err := db.ActiveWorkerCount(ctx, project.ID)
	if err != nil || count != 0 {
		t.Fatalf("queued Task consumes a Worker slot: count=%d err=%v", count, err)
	}
	if err := db.Transition(ctx, id, StateQueued, StateSpawning, "cli", "slot available"); err != nil {
		t.Fatalf("queued Task cannot be dispatched: %v", err)
	}
}

func TestQuietPROpenedAckIsAtomicRetryableAndLeavesClaimedNotices(t *testing.T) {
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
	other, err := db.CreateProject(ctx, "other", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "pr_opened", Summary: "opened"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, Notice{ProjectID: other.ID, Kind: "pr_opened", Summary: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "pr_watch_failing", Summary: "requires Lead"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimNoticeBatch(ctx, project.ID, []int64{id}, "owner", 1)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if count, err := db.AckUndeliveredPROpened(ctx, project.ID, 2); err != nil || count != 0 {
		t.Fatalf("quiet ack stole claim: count=%d err=%v", count, err)
	}
	if err := db.RollbackNoticeClaim(ctx, project.ID, []int64{id}, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_quiet_ack BEFORE UPDATE OF acked_at ON notices BEGIN SELECT RAISE(FAIL, 'simulated ack failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AckUndeliveredPROpened(ctx, project.ID, 3); err == nil {
		t.Fatal("quiet ack failure was ignored")
	}
	pending, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(pending) != 2 {
		t.Fatalf("failed ack lost Notices: %+v %v", pending, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER reject_quiet_ack`); err != nil {
		t.Fatal(err)
	}
	if count, err := db.AckUndeliveredPROpened(ctx, project.ID, 4); err != nil || count != 1 {
		t.Fatalf("retry count=%d err=%v", count, err)
	}
	if count, err := db.AckUndeliveredPROpened(ctx, project.ID, 5); err != nil || count != 0 {
		t.Fatalf("duplicate quiet ack count=%d err=%v", count, err)
	}
	all, err := db.Notices(ctx, other.ID, true)
	if err != nil || len(all) != 1 {
		t.Fatalf("other Project's Notice was acked: %+v %v", all, err)
	}
}

func TestRequeueNoticesRetriesOnlyUnacknowledgedDeliveredBatch(t *testing.T) {
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
	id, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "needs_decision", Summary: "choose"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkNoticesDelivered(ctx, project.ID, []int64{id}, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueNotices(ctx, project.ID, []int64{id}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("failed delivery not retried: %+v %v", pending, err)
	}
	if _, err := db.AckNotices(ctx, project.ID, []string{fmt.Sprint(id)}); err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueNotices(ctx, project.ID, []int64{id}); err != nil {
		t.Fatal(err)
	}
	pending, err = db.UndeliveredNotices(ctx, project.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged Notice was requeued: %+v %v", pending, err)
	}
}

func TestPreexistingQueuedMessagesRetainWaitForIdle(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(context.Background(), project.ID, Task{Seq: 1, Type: "change", Title: "Old", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO messages(task_id, body, created_at, status) VALUES (?, 'old instruction', 1, 'queued')`, id); err != nil {
		t.Fatal(err)
	}
	message, err := db.OldestQueuedMessage(context.Background(), id)
	if err != nil || !message.WaitForIdle {
		t.Fatalf("legacy message=%#v err=%v", message, err)
	}
}

func TestStateTransitionsAreCompareAndSetAndAudited(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Fix", MergeMode: "local", AutonomyReview: "ask", AutonomyMerge: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "agent ready"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateDone, "herdr", "agent went idle"); err == nil {
		t.Fatal("Herdr source was allowed to mark a Task done")
	}
	if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "Signal done"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateFailed, "worker", "stale state"); err == nil {
		t.Fatal("compare-and-set accepted an obsolete state")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE task_id=?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("transition audit rows = %d, want initial + two successful transitions", count)
	}
	transitions, err := db.TaskTransitions(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantSources := []string{"worker", "cli", "cli"}
	for index, transition := range transitions {
		if transition.Source != wantSources[index] {
			t.Errorf("transition %d source = %q, want %q", index, transition.Source, wantSources[index])
		}
	}
}

func TestApprovedDiscardSourceStatesUseOnePredicate(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	allowed := []State{StateSpawning, StateWorking, StateNeedsDecision, StateBlocked, StateStalled, StateDone, StateMerging, StateFailed, StateLost, StateReported, StateMerged}
	for index, state := range allowed {
		t.Run(string(state), func(t *testing.T) {
			if !IsApprovedDiscardSourceState(state) {
				t.Fatalf("%s is not an approved-discard source", state)
			}
			if err := validateTransitionSource(state, StateTornDown, "user"); err != nil {
				t.Fatalf("approved transition source rejected for %s: %v", state, err)
			}
			id, err := db.CreateTask(ctx, project.ID, Task{Seq: index + 1, Type: "research", Title: string(state), MergeMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			if state != StateSpawning {
				if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case StateSpawning, StateWorking:
			case StateNeedsDecision:
				err = db.Transition(ctx, id, StateWorking, state, "worker", "needs decision")
			case StateBlocked:
				err = db.Transition(ctx, id, StateWorking, state, "herdr", "blocked")
			case StateDone:
				err = db.Transition(ctx, id, StateWorking, state, "worker", "done")
			case StateMerging:
				if err = db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err == nil {
					err = db.Transition(ctx, id, StateDone, state, "cli", "merging")
				}
			case StateFailed:
				err = db.Transition(ctx, id, StateWorking, state, "worker", "failed")
			case StateLost:
				err = db.Transition(ctx, id, StateWorking, state, "cli", "lost")
			case StateStalled:
				err = db.Transition(ctx, id, StateWorking, state, "cli", "stalled")
			case StateReported:
				if err = db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err == nil {
					err = db.Transition(ctx, id, StateDone, state, "cli", "reported")
				}
			case StateMerged:
				if err = db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err == nil {
					err = db.Transition(ctx, id, StateDone, StateMerging, "cli", "merging")
				}
				if err == nil {
					err = db.Transition(ctx, id, StateMerging, state, "cli", "merged")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if state == StateReported || state == StateMerged {
				if err := validateTransitionSource(state, StateTornDown, "cli"); err != nil {
					t.Fatalf("ordinary completion source rejected for %s: %v", state, err)
				}
			} else if err := validateTransitionSource(state, StateTornDown, "cli"); err == nil {
				t.Fatalf("ordinary teardown source accepted from unmerged %s", state)
			}
			if err := db.Transition(ctx, id, state, StateTornDown, "user", "discarded without recording approval"); err == nil {
				t.Fatalf("Transition bypassed approval from %s", state)
			}
			if err := db.RecordApproval(ctx, id, "discard", "User approved discard"); err != nil {
				t.Fatal(err)
			}
			if err := db.TransitionWithApproval(ctx, id, state, StateTornDown, "user", "discarded", "merge", "User approved discard"); !errors.Is(err, ErrInvalidApprovedDiscardTransition) {
				t.Fatalf("TransitionWithApproval accepted non-discard action from %s: %v", state, err)
			}
			if err := db.TransitionAfterApproval(ctx, id, state, StateTornDown, "user", "discarded", "merge"); !errors.Is(err, ErrInvalidApprovedDiscardTransition) {
				t.Fatalf("TransitionAfterApproval accepted non-discard action from %s: %v", state, err)
			}
			if err := db.TransitionAfterApproval(ctx, id, state, StateTornDown, "user", "discarded", "discard"); err != nil {
				t.Fatalf("approved transition from %s: %v", state, err)
			}
		})
	}

	for _, state := range []State{StateTornDown} {
		t.Run("refused_"+string(state), func(t *testing.T) {
			if IsApprovedDiscardSourceState(state) {
				t.Fatalf("%s unexpectedly qualifies for approved discard", state)
			}
			if err := validateTransitionSource(state, StateTornDown, "user"); err == nil {
				t.Fatalf("approved transition source accepted from %s", state)
			}
			if err := db.TransitionAfterApproval(ctx, 0, state, StateTornDown, "user", "discarded", "discard"); !errors.Is(err, ErrInvalidApprovedDiscardTransition) {
				t.Fatalf("TransitionAfterApproval(%s) = %v, want deterministic refusal", state, err)
			}
			if err := db.TransitionWithApproval(ctx, 0, state, StateTornDown, "user", "discarded", "discard", "User approved discard"); !errors.Is(err, ErrInvalidApprovedDiscardTransition) {
				t.Fatalf("TransitionWithApproval(%s) = %v, want deterministic refusal", state, err)
			}
		})
	}
}

func TestTerminalTaskTransitionsMakeQueuedMessagesUndeliverable(t *testing.T) {
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

	tests := []struct {
		name     string
		taskType string
		followUp string
		finish   func(int64) error
	}{
		{name: "reported", taskType: "research", followUp: "Create a new Change Task", finish: func(id int64) error {
			if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
				return err
			}
			return db.Transition(ctx, id, StateDone, StateReported, "cli", "report copied")
		}},
		{name: "failed", taskType: "change", followUp: "posse relaunch t2", finish: func(id int64) error {
			return db.Transition(ctx, id, StateWorking, StateFailed, "worker", "failed")
		}},
		{name: "lost", taskType: "change", followUp: "posse relaunch t3", finish: func(id int64) error {
			return db.Transition(ctx, id, StateWorking, StateLost, "cli", "Worker disappeared")
		}},
		{name: "merged", taskType: "change", followUp: "Create a new Change Task", finish: func(id int64) error {
			if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
				return err
			}
			if err := db.Transition(ctx, id, StateDone, StateMerging, "cli", "Gate passed"); err != nil {
				return err
			}
			return db.Transition(ctx, id, StateMerging, StateMerged, "cli", "merged")
		}},
		{name: "torn-down", taskType: "change", followUp: "Create a new Change Task", finish: func(id int64) error {
			if err := db.Transition(ctx, id, StateWorking, StateStalled, "cli", "Worker stalled"); err != nil {
				return err
			}
			return db.TransitionWithApproval(ctx, id, StateStalled, StateTornDown, "user", "discard", "discard", "User approved discard")
		}},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, err := db.CreateTask(ctx, project.ID, Task{Seq: index + 1, Type: test.taskType, Title: test.name, MergeMode: "local"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "Worker ready"); err != nil {
				t.Fatal(err)
			}
			messageID, err := db.QueueMessage(ctx, id, "queued before "+test.name, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.finish(id); err != nil {
				t.Fatal(err)
			}
			if _, err := db.QueueMessage(ctx, id, "too late", true); !errors.Is(err, ErrStateRace) {
				t.Fatalf("terminal Task accepted another queued message: %v", err)
			}
			message, err := db.MessageByID(ctx, messageID)
			if err != nil || message.Status != "undeliverable" {
				t.Fatalf("message after %s transition = %#v, %v", test.name, message, err)
			}
			messages, err := db.UndeliveredTaskMessages(ctx, id)
			if err != nil || len(messages) != 1 || messages[0].ID != messageID {
				t.Fatalf("showable undelivered messages = %#v, %v", messages, err)
			}
			notices, err := db.Notices(ctx, project.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, notice := range notices {
				if notice.TaskID == id && notice.Kind == "queued_message_undeliverable" {
					found++
					if !strings.Contains(notice.Summary, "#"+fmt.Sprint(messageID)) || !strings.Contains(notice.Summary, test.followUp) {
						t.Fatalf("queued-message Notice lacks its ID or supported follow-up: %#v", notice)
					}
				}
			}
			if found != 1 {
				t.Fatalf("queued-message Notices = %d, want one naming message #%d: %#v", found, messageID, notices)
			}
		})
	}
}

func latestEmbeddedMigrationVersion(t *testing.T) int64 {
	t.Helper()
	sources, err := embeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var latest int64
	for _, source := range sources {
		if source.version > latest {
			latest = source.version
		}
	}
	return latest
}

func TestOpenAtAppliesGooseMigrations(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != latestEmbeddedMigrationVersion(t) {
		t.Fatalf("applied Goose migration version = %d, want %d", version, latestEmbeddedMigrationVersion(t))
	}
}

func TestOpenAddsProjectUUIDToVersion32Home(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX projects_project_uuid_idx`,
		`ALTER TABLE projects DROP COLUMN project_uuid`,
		`DELETE FROM goose_db_version WHERE version_id=33`,
		`DELETE FROM posse_migration_checksums WHERE version=33`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare main version 32 database: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open main version 32 database: %v", err)
	}
	defer db.Close()
	migrated, err := db.ProjectByID(ctx, project.ID)
	if err != nil || migrated.UUID == "" {
		t.Fatalf("Project UUID after migration = %q, %v; want a generated UUID", migrated.UUID, err)
	}
	var applied, index int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=33 AND is_applied=1`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("version 33 applied rows = %d, %v; want 1", applied, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='projects_project_uuid_idx'`).Scan(&index); err != nil || index != 1 {
		t.Fatalf("Project UUID index count = %d, %v; want 1", index, err)
	}
}

func TestOpenMigratesHomeWithLegacyProjectUUIDVersion32(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	legacyUUID := "f0a17a02-cb03-4371-89ab-544a51e36744"
	for _, statement := range []string{
		`ALTER TABLE project_watch_state DROP COLUMN pr_poll_last_error`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_block_notice_raised`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_block_cause`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_blocked_since`,
		`DROP TABLE task_spec_checks`,
		`DROP TABLE ci_reruns`,
		`DROP TABLE ci_test_failures`,
		`DROP TABLE task_skill_uses`,
		`DROP TABLE remote_branch_cleanups`,
		`ALTER TABLE project_runtime DROP COLUMN lead_agent_session`,
		`ALTER TABLE project_runtime DROP COLUMN lead_agent_kind`,
		`DROP TABLE interventions`,
		`DROP TABLE quota_exhaustions`,
		`DROP TABLE delivery_operations`,
		`DROP TABLE deliveries`,
		`ALTER TABLE pr_observations DROP COLUMN confirmed_at`,
		`ALTER TABLE tasks DROP COLUMN agent_status_since`,
		`ALTER TABLE tasks DROP COLUMN agent_status`,
		`DROP TABLE project_lookout_pollers`,
		`DROP TABLE report_acceptances`,
		`DROP INDEX report_reviews_target_idx`,
		`DROP TABLE report_reviews`,
		`ALTER TABLE tasks DROP COLUMN report_manifest_json`,
		`ALTER TABLE tasks DROP COLUMN report_digest`,
		`ALTER TABLE tasks DROP COLUMN report_kind`,
		`DROP TABLE task_plan_revisions`,
		`ALTER TABLE tasks DROP COLUMN plan_json`,
		`ALTER TABLE task_launch_identities DROP COLUMN plan_revision`,
		`DROP TABLE task_target_changes`,
		`DROP TABLE model_error_episodes`,
		`DROP TABLE publish_pre_push_heads`,
		`DROP TABLE notice_delivery_receipts`,
		`DROP TRIGGER project_history_revision_decision_delete`,
		`DROP TRIGGER project_history_revision_decision_update`,
		`DROP TRIGGER project_history_revision_decision_insert`,
		`DROP TRIGGER project_history_revision_notice_delete`,
		`DROP TRIGGER project_history_revision_notice_update`,
		`DROP TRIGGER project_history_revision_notice_insert`,
		`DROP TRIGGER project_history_revision_project_insert`,
		`DROP TABLE project_history_revisions`,
		`DROP INDEX notices_task_kind_idx`,
		`DROP INDEX notices_sandbox_episode_idx`,
		`DROP INDEX notices_project_id_idx`,
		`ALTER TABLE pr_body_markers DROP COLUMN validation_digest`,
		`DROP TABLE merge_queue_entries`,
		`DROP TABLE research_attempts`,
		`DROP TABLE member_watch_errors`,
		`DROP TABLE validation_stage_results`,
		`DROP TABLE validation_candidates`,
		`DROP TABLE task_turn_tokens`,
		`DROP INDEX projects_project_uuid_idx`,
		`ALTER TABLE projects DROP COLUMN project_uuid`,
		`ALTER TABLE tasks DROP COLUMN agent_session_kind`,
		`ALTER TABLE task_repos DROP COLUMN target_branch`,
		`ALTER TABLE tasks DROP COLUMN target_branch`,
		`ALTER TABLE tasks DROP COLUMN pr_title`,
		`DROP TABLE pr_task_watch_failures`,
		`DROP TABLE repo_fetch_failures`,
		`DROP TABLE completed_rider_busy`,
		`DROP TABLE resource_leases`,
		`DROP TABLE review_receipts`,
		`DROP TABLE review_verdicts`,
		`DROP TABLE review_targets`,
		`ALTER TABLE tasks DROP COLUMN containment_marker`,
		`ALTER TABLE tasks DROP COLUMN containment_path`,
		`ALTER TABLE tasks DROP COLUMN containment_mode`,
		`DROP TABLE validation_fix_rounds`,
		`DROP TABLE review_dispositions`,
		`DROP TABLE review_carries`,
		`DROP TABLE review_finding_events`,
		`DROP TABLE review_findings`,
		`DROP TABLE review_patterns`,
		`DROP TABLE review_rounds`,
		`ALTER TABLE tasks DROP COLUMN pane_shell_identity`,
		`ALTER TABLE lead_start_claims DROP COLUMN pid`,
		`ALTER TABLE lead_start_claims DROP COLUMN holder`,
		`DELETE FROM goose_db_version WHERE version_id >= 32`,
		`DELETE FROM posse_migration_checksums WHERE version >= 32`,
		`ALTER TABLE projects ADD COLUMN project_uuid TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX projects_project_uuid_idx ON projects(project_uuid)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare legacy project UUID database: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE projects SET project_uuid=? WHERE id=?`, legacyUUID, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO goose_db_version(version_id,is_applied) VALUES(32,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(32,?)`, legacyProjectUUIDMigrationChecksum); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	previousGate := MigrationGate
	MigrationGate = func(_ string, pending []int64, _ bool) error {
		if len(pending) != 1 || pending[0] != 32 {
			t.Fatalf("legacy migration gate pending versions = %v, want [32]", pending)
		}
		return &SchemaError{Code: "migration_refused", Message: "test refusal"}
	}
	t.Cleanup(func() { MigrationGate = previousGate })
	if refused, err := OpenAt(path); err == nil {
		_ = refused.Close()
		t.Fatal("legacy migration ignored the migration gate")
	}
	MigrationGate = previousGate

	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open home migrated by the previous Project UUID migration: %v", err)
	}
	defer db.Close()
	migrated, err := db.ProjectByID(ctx, project.ID)
	if err != nil || migrated.UUID != legacyUUID {
		t.Fatalf("legacy Project UUID after migration = %q, %v; want %q", migrated.UUID, err, legacyUUID)
	}
	for _, version := range []int64{32, 33} {
		var applied int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, version).Scan(&applied); err != nil || applied != 1 {
			t.Fatalf("migration %d applied rows = %d, %v; want 1", version, applied, err)
		}
	}
	var prePushHeads int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='publish_pre_push_heads'`).Scan(&prePushHeads); err != nil || prePushHeads != 1 {
		t.Fatalf("publish_pre_push_heads table count = %d, %v; want 1", prePushHeads, err)
	}
}

func TestOpenMigratesLegacyNoticeDeliveryMigrationVersion32(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "needs_decision", Summary: "receipt migration"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, claimed, err := db.ClaimNoticeDelivery(ctx, project.ID, []int64{noticeID}, "pi:test", "generation", "owner", 100, 1000)
	if err != nil || !claimed {
		t.Fatalf("claim legacy receipt = %+v, %v, %v", delivery, claimed, err)
	}
	for _, statement := range []string{
		`ALTER TABLE project_watch_state DROP COLUMN pr_poll_last_error`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_block_notice_raised`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_block_cause`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_blocked_since`,
		`DROP TABLE task_spec_checks`,
		`DROP TABLE ci_reruns`,
		`DROP TABLE ci_test_failures`,
		`DROP TABLE task_skill_uses`,
		`DROP TABLE remote_branch_cleanups`,
		`ALTER TABLE project_runtime DROP COLUMN lead_agent_session`,
		`ALTER TABLE project_runtime DROP COLUMN lead_agent_kind`,
		`DROP TABLE interventions`,
		`DROP TABLE quota_exhaustions`,
		`DROP TABLE delivery_operations`,
		`DROP TABLE deliveries`,
		`ALTER TABLE pr_observations DROP COLUMN confirmed_at`,
		`ALTER TABLE tasks DROP COLUMN agent_status_since`,
		`ALTER TABLE tasks DROP COLUMN agent_status`,
		`DROP TABLE project_lookout_pollers`,
		`DROP TABLE report_acceptances`,
		`DROP INDEX report_reviews_target_idx`,
		`DROP TABLE report_reviews`,
		`ALTER TABLE tasks DROP COLUMN report_manifest_json`,
		`ALTER TABLE tasks DROP COLUMN report_digest`,
		`ALTER TABLE tasks DROP COLUMN report_kind`,
		`DROP TABLE task_plan_revisions`,
		`ALTER TABLE tasks DROP COLUMN plan_json`,
		`ALTER TABLE task_launch_identities DROP COLUMN plan_revision`,
		`DROP TABLE task_target_changes`,
		`DROP TABLE model_error_episodes`,
		`DROP TABLE publish_pre_push_heads`,
		`DROP INDEX projects_project_uuid_idx`,
		`ALTER TABLE projects DROP COLUMN project_uuid`,
		`ALTER TABLE tasks DROP COLUMN agent_session_kind`,
		`ALTER TABLE task_repos DROP COLUMN target_branch`,
		`ALTER TABLE tasks DROP COLUMN target_branch`,
		`ALTER TABLE tasks DROP COLUMN pr_title`,
		`DROP TABLE pr_task_watch_failures`,
		`DROP TABLE repo_fetch_failures`,
		`DROP TABLE completed_rider_busy`,
		`DROP TABLE resource_leases`,
		`DROP TABLE review_receipts`,
		`DROP INDEX notice_delivery_active_destination_idx`,
		`CREATE UNIQUE INDEX notice_delivery_one_active_destination_idx ON notice_delivery_receipts(project_id, destination) WHERE state IN ('claimed','printed')`,
		`DROP TRIGGER project_history_revision_decision_delete`,
		`DROP TRIGGER project_history_revision_decision_update`,
		`DROP TRIGGER project_history_revision_decision_insert`,
		`DROP TRIGGER project_history_revision_notice_delete`,
		`DROP TRIGGER project_history_revision_notice_update`,
		`DROP TRIGGER project_history_revision_notice_insert`,
		`DROP TRIGGER project_history_revision_project_insert`,
		`DROP TABLE project_history_revisions`,
		`DROP INDEX notices_task_kind_idx`,
		`DROP INDEX notices_sandbox_episode_idx`,
		`DROP INDEX notices_project_id_idx`,
		`ALTER TABLE pr_body_markers DROP COLUMN validation_digest`,
		`DROP TABLE merge_queue_entries`,
		`DROP TABLE research_attempts`,
		`DROP TABLE member_watch_errors`,
		`DROP TABLE validation_stage_results`,
		`DROP TABLE validation_candidates`,
		`DROP TABLE review_verdicts`,
		`DROP TABLE review_targets`,
		`ALTER TABLE tasks DROP COLUMN containment_marker`,
		`ALTER TABLE tasks DROP COLUMN containment_path`,
		`ALTER TABLE tasks DROP COLUMN containment_mode`,
		`DROP TABLE validation_fix_rounds`,
		`DROP TABLE review_dispositions`,
		`DROP TABLE review_carries`,
		`DROP TABLE review_finding_events`,
		`DROP TABLE review_findings`,
		`DROP TABLE review_patterns`,
		`DROP TABLE review_rounds`,
		`ALTER TABLE tasks DROP COLUMN pane_shell_identity`,
		`DROP TABLE task_turn_tokens`,
		`ALTER TABLE lead_start_claims DROP COLUMN pid`,
		`ALTER TABLE lead_start_claims DROP COLUMN holder`,
		`DELETE FROM goose_db_version WHERE version_id >= 32`,
		`DELETE FROM posse_migration_checksums WHERE version >= 32`,
		`INSERT INTO goose_db_version(version_id,is_applied) VALUES(32,1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare legacy receipt migration: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO posse_migration_checksums(version,sha256) VALUES(32,?)`, legacyNoticeDeliveryMigrationChecksum); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("open legacy receipt database: %v", err)
	}
	defer db.Close()
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil || project.UUID == "" {
		t.Fatalf("Project UUID after migration = %q, %v", project.UUID, err)
	}
	restored, err := db.NoticeDelivery(ctx, delivery.DeliveryID)
	if err != nil || restored.State != "claimed" {
		t.Fatalf("legacy receipt after migration = %+v, %v", restored, err)
	}
	for _, version := range []int64{32, 33, 34, 35} {
		var applied int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, version).Scan(&applied); err != nil || applied != 1 {
			t.Fatalf("migration %d applied rows = %d, %v; want 1", version, applied, err)
		}
	}
}

func TestConfigApprovalMoveMigrationPreservesExistingApprovals(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConfigApproval(ctx, project.ID, "autonomy.merge", "set", "auto", "Use automatic Merge"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConfigApproval(ctx, project.ID, "autonomy.review", "unset", "", "Clear review Autonomy"); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE config_approvals_v26 (
			id INTEGER PRIMARY KEY,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			key TEXT NOT NULL,
			action TEXT NOT NULL CHECK (action IN ('set', 'unset')),
			value TEXT,
			user_quote TEXT NOT NULL CHECK (length(trim(user_quote)) > 0),
			at INTEGER NOT NULL,
			CHECK ((action = 'set' AND value IS NOT NULL) OR (action = 'unset' AND value IS NULL))
		)`,
		`INSERT INTO config_approvals_v26 SELECT * FROM config_approvals`,
		`DROP TABLE config_approvals`,
		`ALTER TABLE config_approvals_v26 RENAME TO config_approvals`,
		`DELETE FROM goose_db_version WHERE version_id = 27`,
		`DELETE FROM posse_migration_checksums WHERE version = 27`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatalf("apply move-approval migration: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT project_id,key,action,value,user_quote FROM config_approvals ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type approval struct {
		projectID int64
		key       string
		action    string
		value     sql.NullString
		quote     string
	}
	var approvals []approval
	for rows.Next() {
		var item approval
		if err := rows.Scan(&item.projectID, &item.key, &item.action, &item.value, &item.quote); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		approvals = append(approvals, item)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 2 || approvals[0].projectID != project.ID || approvals[0].key != "autonomy.merge" || approvals[0].action != "set" || approvals[0].value.String != "auto" || !approvals[0].value.Valid || approvals[0].quote != "Use automatic Merge" || approvals[1].projectID != project.ID || approvals[1].key != "autonomy.review" || approvals[1].action != "unset" || approvals[1].value.Valid || approvals[1].quote != "Clear review Autonomy" {
		t.Fatalf("historical approvals after migration = %#v", approvals)
	}
	if err := db.RecordConfigApproval(ctx, project.ID, "preferences.lead", "move", filepath.Join("/home", "preferences", "lead.md"), "Move my Lead preferences"); err != nil {
		t.Fatalf("record move approval after migration: %v", err)
	}
}

func TestTerminalMessageMigrationRepairsExistingOrphans(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Legacy report", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "Worker ready"); err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "legacy queued instruction", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER terminalize_queued_messages`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='reported' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE project_watch_state DROP COLUMN pr_poll_last_error`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_block_notice_raised`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_block_cause`,
		`ALTER TABLE project_watch_state DROP COLUMN reconcile_blocked_since`,
		`DROP TABLE task_spec_checks`,
		`DROP TABLE ci_reruns`,
		`DROP TABLE ci_test_failures`,
		`DROP TABLE task_skill_uses`,
		`DROP TABLE remote_branch_cleanups`,
		`ALTER TABLE project_runtime DROP COLUMN lead_agent_session`,
		`ALTER TABLE project_runtime DROP COLUMN lead_agent_kind`,
		`DROP TABLE interventions`,
		`DROP TABLE quota_exhaustions`,
		`DROP TABLE delivery_operations`,
		`DROP TABLE deliveries`,
		`ALTER TABLE pr_observations DROP COLUMN confirmed_at`,
		`ALTER TABLE tasks DROP COLUMN agent_status_since`,
		`ALTER TABLE tasks DROP COLUMN agent_status`,
		`DROP TABLE project_lookout_pollers`,
		`DROP TABLE report_acceptances`,
		`DROP INDEX report_reviews_target_idx`,
		`DROP TABLE report_reviews`,
		`ALTER TABLE tasks DROP COLUMN report_manifest_json`,
		`ALTER TABLE tasks DROP COLUMN report_digest`,
		`ALTER TABLE tasks DROP COLUMN report_kind`,
		`DROP TABLE task_plan_revisions`,
		`ALTER TABLE tasks DROP COLUMN plan_json`,
		`ALTER TABLE task_launch_identities DROP COLUMN plan_revision`,
		`DROP TABLE task_target_changes`,
		`ALTER TABLE repo_watch_state DROP COLUMN checkout_reason`,
		`ALTER TABLE repo_watch_state DROP COLUMN checkout_status`,
		`ALTER TABLE repo_watch_state DROP COLUMN checkout_checked_at`,
		`ALTER TABLE project_repos DROP COLUMN origin_host`,
		`DROP TABLE member_pr_poll_state`,
		`DROP TRIGGER project_history_revision_decision_delete`,
		`DROP TRIGGER project_history_revision_decision_update`,
		`DROP TRIGGER project_history_revision_decision_insert`,
		`DROP TRIGGER project_history_revision_notice_delete`,
		`DROP TRIGGER project_history_revision_notice_update`,
		`DROP TRIGGER project_history_revision_notice_insert`,
		`DROP TRIGGER project_history_revision_project_insert`,
		`DROP TABLE project_history_revisions`,
		`DROP INDEX notices_task_kind_idx`,
		`DROP INDEX notices_sandbox_episode_idx`,
		`DROP INDEX notices_project_id_idx`,
		`ALTER TABLE pr_body_markers DROP COLUMN validation_digest`,
		`DROP TABLE merge_queue_entries`,
		`DROP TABLE research_attempts`,
		`DROP TABLE member_watch_errors`,
		`DROP TABLE validation_stage_results`,
		`DROP TABLE validation_candidates`,
		`DROP TABLE notice_delivery_receipts`,
		`DROP TABLE pr_body_markers`,
		`DROP TABLE model_error_episodes`,
		`DROP INDEX projects_project_uuid_idx`,
		`ALTER TABLE projects DROP COLUMN project_uuid`,
		`DROP TABLE publish_pre_push_heads`,
		`ALTER TABLE tasks DROP COLUMN agent_session_kind`,
		`ALTER TABLE task_repos DROP COLUMN target_branch`,
		`ALTER TABLE tasks DROP COLUMN target_branch`,
		`ALTER TABLE tasks DROP COLUMN pr_title`,
		`DROP TABLE pr_task_watch_failures`,
		`DROP TABLE repo_fetch_failures`,
		`DROP TABLE completed_rider_busy`,
		`DROP TABLE resource_leases`,
		`DROP TABLE review_receipts`,
		`DROP TABLE review_verdicts`,
		`DROP TABLE review_targets`,
		`ALTER TABLE tasks DROP COLUMN containment_marker`,
		`ALTER TABLE tasks DROP COLUMN containment_path`,
		`ALTER TABLE tasks DROP COLUMN containment_mode`,
		`DROP TABLE validation_fix_rounds`,
		`DROP TABLE review_dispositions`,
		`DROP TABLE review_carries`,
		`DROP TABLE review_finding_events`,
		`DROP TABLE review_findings`,
		`DROP TABLE review_patterns`,
		`DROP TABLE review_rounds`,
		`ALTER TABLE tasks DROP COLUMN pane_shell_identity`,
		`DROP TABLE task_turn_tokens`,
		`ALTER TABLE lead_start_claims DROP COLUMN pid`,
		`ALTER TABLE lead_start_claims DROP COLUMN holder`,
		`DELETE FROM goose_db_version WHERE version_id >= 26`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare pre-migration database: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	message, err := db.MessageByID(ctx, messageID)
	if err != nil || message.Status != "undeliverable" {
		t.Fatalf("legacy orphan after migration = %#v, %v", message, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "queued_message_undeliverable" || !strings.Contains(notices[0].Summary, "#"+fmt.Sprint(messageID)) {
		t.Fatalf("migration did not create its repair Notice: %#v, %v", notices, err)
	}
}

func TestMigrationMakesLegacyMessageClaimsNonRetryable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Fix", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(ctx, taskID, "possibly submitted before upgrade", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET status='claimed',claim_token='legacy',claimed_at=1 WHERE id=?`, messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id=21`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ReleaseExpiredDeliveryClaims(ctx, 2); err != nil {
		t.Fatal(err)
	}
	message, err := db.MessageByID(ctx, messageID)
	if err != nil || message.Status != "submitting" {
		t.Fatalf("legacy claim was made retryable: %#v, %v", message, err)
	}
	submissions, err := db.ExpiredMessageSubmissions(ctx, project.ID, 0, 2)
	if err != nil || len(submissions) != 1 || submissions[0].ID != messageID {
		t.Fatalf("legacy submission was not found for recovery: %#v, %v", submissions, err)
	}
	created, err := db.CreateMessageDeliveryUncertainNotice(ctx, project.ID, taskID, messageID, "message #1 may not have reached Worker t1", message.Body)
	if err != nil || !created {
		t.Fatalf("legacy submission Notice was not created: created=%t err=%v", created, err)
	}
	created, err = db.CreateMessageDeliveryUncertainNotice(ctx, project.ID, taskID, messageID, "duplicate summary", message.Body)
	if err != nil || created {
		t.Fatalf("legacy submission Notice was not deduplicated: created=%t err=%v", created, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "message_delivery_uncertain" {
		t.Fatalf("legacy submission Notices = %#v, %v", notices, err)
	}
	if err := db.MarkClaimedMessageDelivered(ctx, messageID, "legacy", 3); err != nil {
		t.Fatal(err)
	}
	notices, err = db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].AckedAt != 3 {
		t.Fatalf("confirmed delivery did not resolve its uncertainty Notice: %#v, %v", notices, err)
	}
}

func TestPendingMergeReadyDecisionMigrationAddsDiscard(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "ready to Merge", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to State
		source   string
	}{{StateSpawning, StateWorking, "cli"}, {StateWorking, StateDone, "worker"}, {StateDone, StateMerging, "cli"}} {
		if err := db.Transition(ctx, taskID, transition.from, transition.to, transition.source, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	decision, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "notice:1", Kind: "merge_ready", Question: "Merge the Task?", Options: []string{"merge", "wait"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE decisions SET kind='land_ready',options_json='["land","wait"]' WHERE id=?`, decision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id IN (52,55)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(home)
	if err != nil {
		t.Fatalf("apply merge_ready option migration: %v", err)
	}
	defer db.Close()
	migrated, err := db.Decision(ctx, project.ID, decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(migrated.Options, ","); got != "merge,wait,discard" {
		t.Fatalf("migrated merge_ready options = %q", got)
	}
}

func TestOpenAtAppliesMissingMigrationBelowCurrentVersion(t *testing.T) {
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1`).Scan(&version); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if version != latestEmbeddedMigrationVersion(t) {
		db.Close()
		t.Fatalf("initial Goose migration version = %d, want %d", version, latestEmbeddedMigrationVersion(t))
	}
	if _, err := db.ExecContext(context.Background(), `ALTER TABLE messages DROP COLUMN wait_for_idle`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM goose_db_version WHERE version_id = 12`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	var missing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id = 12 AND is_applied = 1`).Scan(&missing); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if missing != 0 {
		db.Close()
		t.Fatalf("migration 12 was not removed from the test database: %d rows", missing)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(home)
	if err != nil {
		t.Fatalf("opening a database missing migration 12: %v", err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id = 12 AND is_applied = 1`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 1 {
		t.Fatalf("migration 12 applied rows = %d, want 1", missing)
	}
	if err := db.QueryRow(`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != latestEmbeddedMigrationVersion(t) {
		t.Fatalf("reopened Goose migration version = %d, want %d", version, latestEmbeddedMigrationVersion(t))
	}
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	foundColumn := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "wait_for_idle" {
			foundColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundColumn {
		t.Fatal("migration 12 did not restore messages.wait_for_idle")
	}
}

func TestTaskShortNamePersistsThroughDatabaseAndSnapshot(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Fix", ShortName: "worker-tree", MergeMode: "local", TargetBranch: "staging"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil || task.ShortName != "worker-tree" || task.TargetBranch != "staging" {
		t.Fatalf("Task short name or target branch = %q / %q, %v", task.ShortName, task.TargetBranch, err)
	}
	data, err := os.ReadFile(db.TaskSnapshotPath(project.Name, task.Seq))
	if err != nil || !strings.Contains(string(data), "short_name = \"worker-tree\"") || !strings.Contains(string(data), "target_branch = \"staging\"") {
		t.Fatalf("Task snapshot short name or target branch missing: %q, %v", data, err)
	}
}

func TestConcurrentOpenWaitsForSchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	const openers = 12
	start := make(chan struct{})
	errors := make(chan error, openers)
	var group sync.WaitGroup
	for range openers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			db, err := OpenAt(path)
			if err == nil {
				err = db.Close()
			}
			errors <- err
		}()
	}
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent OpenAt failed: %v", err)
		}
	}
}

func TestProjectRecoveryClaimCompareAndSet(t *testing.T) {
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
	claimed, previous, err := db.ClaimProjectRecovery(ctx, project.ID, "generation-1", 101, 10, 0)
	if err != nil || !claimed || previous.Generation != "" {
		t.Fatalf("initial recovery claim = %v %#v %v", claimed, previous, err)
	}
	claimed, active, err := db.ClaimProjectRecovery(ctx, project.ID, "generation-1", 202, 11, 0)
	if err != nil || claimed || active.OwnerPID != 101 {
		t.Fatalf("overlapping recovery claim = %v %#v %v", claimed, active, err)
	}
	if err := db.FinishProjectRecovery(ctx, project.ID, "generation-1", 101, "", true); err != nil {
		t.Fatal(err)
	}
	claimed, completed, err := db.ClaimProjectRecovery(ctx, project.ID, "generation-1", 202, 12, 0)
	if err != nil || claimed || completed.OwnerPID != 0 {
		t.Fatalf("completed generation was claimed twice = %v %#v %v", claimed, completed, err)
	}
	claimed, previous, err = db.ClaimProjectRecovery(ctx, project.ID, "generation-2", 202, 13, 0)
	if err != nil || !claimed || previous.Generation != "generation-1" {
		t.Fatalf("new generation claim = %v %#v %v", claimed, previous, err)
	}
}

func TestIntentProgressIsOwnedAndClaimedWithCAS(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "intent", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "start", "sandbox.acquire", `{}`, 1001); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "start", "pane.open", `{}`, 1002); !errors.Is(err, ErrIntentOwned) {
		t.Fatalf("second process started an owned intent: %v", err)
	}
	intent, err := db.IntentByTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateIntentStep(ctx, intent.ID, 1001, "pane.open"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimIntent(ctx, intent.ID, 1002, 2001, "recover"); err != nil || claimed {
		t.Fatalf("intent claim ignored the recorded process id: claimed=%t err=%v", claimed, err)
	}
	if claimed, err := db.ClaimIntent(ctx, intent.ID, 1001, 2001, "recover"); err != nil || !claimed {
		t.Fatalf("recovery could not claim the abandoned intent: claimed=%t err=%v", claimed, err)
	}
	if err := db.FinishIntent(ctx, intent.ID, 1001); !errors.Is(err, ErrIntentOwned) {
		t.Fatalf("former owner finished a claimed intent: %v", err)
	}
	if err := db.FinishIntent(ctx, intent.ID, 2001); err != nil {
		t.Fatal(err)
	}
}

func TestIntentStoresProcessBootAndStartIdentity(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "identity", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "start", "started", `{}`, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	intent, err := db.IntentByTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	bootID, startTime, err := ProcessIdentityForPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if intent.ProcessBootID != bootID || intent.ProcessStartTime != startTime {
		t.Fatalf("intent identity = (%q, %q), want (%q, %q)", intent.ProcessBootID, intent.ProcessStartTime, bootID, startTime)
	}
	if claimed, err := db.ClaimIntent(ctx, intent.ID, os.Getpid(), os.Getpid(), "recovered"); err != nil || !claimed {
		t.Fatalf("claim same owner = %t, %v", claimed, err)
	}
	claimedIntent, err := db.IntentByTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if claimedIntent.ProcessBootID != bootID || claimedIntent.ProcessStartTime != startTime {
		t.Fatalf("claimed intent identity = (%q, %q), want (%q, %q)", claimedIntent.ProcessBootID, claimedIntent.ProcessStartTime, bootID, startTime)
	}
}

func TestUnmergedTeardownRequiresRecordedUserApproval(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Fix", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		from, to State
		source   string
	}{{StateSpawning, StateWorking, "cli"}, {StateWorking, StateFailed, "worker"}} {
		if err := db.Transition(ctx, id, step.from, step.to, step.source, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Transition(ctx, id, StateFailed, StateTornDown, "user", "discard"); err == nil {
		t.Fatal("unmerged Task was torn down without an approval record")
	}
	if err := db.TransitionWithApproval(ctx, id, StateFailed, StateTornDown, "user", "discard", "discard", "Please discard this failed Task"); err != nil {
		t.Fatal(err)
	}
	var approvals, transitions int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approvals WHERE task_id=?`, id).Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE task_id=? AND to_state='torn-down'`, id).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	if approvals != 1 || transitions != 1 {
		t.Fatalf("approval and transition counts = %d, %d", approvals, transitions)
	}
}

func TestSignalFromStalledReturnsToWorkingBeforeApplyingTheSignal(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Fix", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateStalled, "cli", "stall detected"); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	state, err := db.RecordWorkerSignal(ctx, task, "needs-decision", "Need the User's answer", nil, "needs_decision")
	if err != nil || state != StateNeedsDecision {
		t.Fatalf("stalled Signal result = %q, %v", state, err)
	}
	transitions, err := db.TaskTransitions(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(transitions) < 2 || transitions[0].From != string(StateWorking) || transitions[0].To != string(StateNeedsDecision) || transitions[0].Source != "worker" || transitions[1].From != string(StateStalled) || transitions[1].To != string(StateWorking) || transitions[1].Source != "worker" {
		t.Fatalf("stalled Signal transition order = %#v", transitions)
	}
	var signals, notices int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM signals WHERE task_id=?`, id).Scan(&signals); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='needs_decision'`, id).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if signals != 1 || notices != 1 {
		t.Fatalf("stalled Signal transaction rows: signals=%d notices=%d", signals, notices)
	}
}

func TestTaskLookupUsesCanonicalPublicIDAndExcludesTornDownPane(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 5, Type: "change", Title: "Fix", MergeMode: "local", PaneID: "w2:p5", PaneLabel: "posse:shop:t5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Task(ctx, project.ID, "5"); !IsNotFound(err) {
		t.Fatalf("bare database or sequence id resolved as a public Task id: %v", err)
	}
	if task, err := db.Task(ctx, project.ID, "t5"); err != nil || task.ID != id {
		t.Fatalf("canonical Task id did not resolve: %#v, %v", task, err)
	}
	if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateDone, StateMerging, "cli", "gate passed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateMerging, StateMerged, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateMerged, StateTornDown, "cli", "released"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TaskByPane(ctx, "w2:p5"); !IsNotFound(err) {
		t.Fatalf("torn-down Worker pane still resolves: %v", err)
	}
}

func TestWorkerSignalAndNoticeRollBackTogether(t *testing.T) {
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Fix", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_task_done_notice BEFORE INSERT ON notices WHEN NEW.kind='task_done' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordWorkerSignal(ctx, task, "done", "committed", nil, "task_done"); err == nil {
		t.Fatal("Signal committed despite Notice failure")
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != StateWorking {
		t.Fatalf("failed Signal changed Task state: %#v, %v", updated, err)
	}
	var signals, notices, transitions int
	for query, destination := range map[string]*int{
		`SELECT COUNT(*) FROM signals WHERE task_id=?`:     &signals,
		`SELECT COUNT(*) FROM notices WHERE task_id=?`:     &notices,
		`SELECT COUNT(*) FROM transitions WHERE task_id=?`: &transitions,
	} {
		if err := db.QueryRowContext(ctx, query, taskID).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	if signals != 0 || notices != 0 || transitions != 2 {
		t.Fatalf("failed Signal left partial records: signals=%d notices=%d transitions=%d", signals, notices, transitions)
	}
}

func TestLeadStartClaimAllowsOnlyOneConcurrentLead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	first, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx := context.Background()
	project, err := first.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	start := make(chan struct{})
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	var group sync.WaitGroup
	for _, db := range []*DB{first, second} {
		group.Add(1)
		go func(db *DB) {
			defer group.Done()
			<-start
			claimed, err := db.ClaimLeadStart(ctx, project.ID, now, now-2*time.Minute.Milliseconds(), "up", os.Getpid())
			results <- claimed
			errors <- err
		}(db)
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)
	claims := 0
	for claimed := range results {
		if claimed {
			claims++
		}
	}
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if claims != 1 {
		t.Fatalf("concurrent Lead claims = %d, want 1", claims)
	}
	claimed, err := second.ClaimLeadStart(ctx, project.ID, now+3*time.Minute.Milliseconds(), now+time.Minute.Milliseconds(), "restore", os.Getpid())
	if err != nil || !claimed {
		t.Fatalf("stale Lead start claim was not recovered = %t, %v", claimed, err)
	}
	claimed, err = first.ClaimLeadStart(ctx, project.ID, now+3*time.Minute.Milliseconds()+1, now+time.Minute.Milliseconds(), "up", os.Getpid())
	if err != nil || claimed {
		t.Fatalf("fresh Lead start claim allowed a second owner = %t, %v", claimed, err)
	}
}

func TestReleaseLeadStartRetriesTransientContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	db, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	locker, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if claimed, err := db.ClaimLeadStart(ctx, project.ID, now, now-2*time.Minute.Milliseconds(), "up", os.Getpid()); err != nil || !claimed {
		t.Fatalf("seed Lead start claim = %t, %v", claimed, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=25`); err != nil {
		t.Fatal(err)
	}
	tx, err := locker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, project.ID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() { released <- db.ReleaseLeadStart(ctx, project.ID, "up", os.Getpid()) }()
	time.Sleep(70 * time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-released; err != nil {
		t.Fatalf("release did not retry after transient contention: %v", err)
	}
	if _, err := db.LeadStartClaim(ctx, project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim after successful retry = %v, want no row", err)
	}
}

func TestReleaseLeadStartReturnsPersistentFailure(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseLeadStart(context.Background(), 1, "up", os.Getpid()); err == nil {
		t.Fatal("ReleaseLeadStart discarded a persistent database failure")
	}
}

func TestLeadStartClaimReclaimsDeadProcessImmediately(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("immediate dead-process reclamation requires procfs")
	}
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	claimed, err := db.ClaimLeadStart(ctx, project.ID, now, now-2*time.Minute.Milliseconds(), "restore", 2147483647)
	if err != nil || !claimed {
		t.Fatalf("seed dead-process claim = %t, %v", claimed, err)
	}
	claimed, err = db.ClaimLeadStart(ctx, project.ID, now+1, now+1-2*time.Minute.Milliseconds(), "up", os.Getpid())
	if err != nil || !claimed {
		t.Fatalf("live caller could not immediately reclaim dead-process claim = %t, %v", claimed, err)
	}
	claim, err := db.LeadStartClaim(ctx, project.ID)
	if err != nil || claim.Holder != "up" || claim.PID != int64(os.Getpid()) {
		t.Fatalf("reclaimed claim = %+v, %v", claim, err)
	}
}

func TestTaskSequenceRejectsReusedBranchWithoutCreatingTask(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	occupied := func(int) (bool, error) { return false, nil }
	if _, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", ShortName: "same-name", Branch: "posse/reused-ref"}, occupied); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", ShortName: "same-name", Branch: "posse/reused-ref"}, occupied); !errors.Is(err, ErrTaskBranchExists) {
		t.Fatalf("reused Task branch error = %v", err)
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil || len(tasks) != 1 || tasks[0].Branch != "posse/reused-ref" {
		t.Fatalf("Tasks after collision = %#v, %v", tasks, err)
	}
}

func TestTaskBranchIsBoundToAllocatedSequence(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	occupied := func(int) (bool, error) { return false, nil }
	if _, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", ShortName: "same-name", Branch: "posse/t2"}, occupied); !errors.Is(err, ErrTaskBranchSequenceMismatch) {
		t.Fatalf("branch for another sequence was admitted: %v", err)
	}
	firstID, firstSeq, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", ShortName: "same-name"}, occupied)
	if err != nil || firstSeq != 1 {
		t.Fatalf("first Task branch allocation = %d, %v", firstSeq, err)
	}
	secondID, secondSeq, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", ShortName: "same-name"}, occupied)
	if err != nil || secondSeq != 2 {
		t.Fatalf("second Task branch allocation = %d, %v", secondSeq, err)
	}
	first, err := db.TaskByID(ctx, project.ID, firstID)
	if err != nil || first.Branch != "posse/t1" || first.ShortName != "same-name" {
		t.Fatalf("first Task identity = %#v, %v", first, err)
	}
	second, err := db.TaskByID(ctx, project.ID, secondID)
	if err != nil || second.Branch != "posse/t2" || second.ShortName != "same-name" {
		t.Fatalf("second Task identity = %#v, %v", second, err)
	}
}

func TestConcurrentTaskSequenceAllocationSerializesAndBothTasksSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "posse.db")
	first, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx := context.Background()
	project, err := first.CreateProject(ctx, "shop", "/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type result struct {
		sequence int
		err      error
	}
	results := make(chan result, 2)
	for index, db := range []*DB{first, second} {
		go func(db *DB, index int) {
			<-start
			_, sequence, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: fmt.Sprintf("Concurrent %d", index), ShortName: fmt.Sprintf("concurrent-%d", index)}, func(int) (bool, error) {
				return false, nil
			})
			results <- result{sequence: sequence, err: err}
		}(db, index)
	}
	close(start)
	seen := map[int]bool{}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent Task allocation failed: %v", result.err)
		}
		seen[result.sequence] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("allocated sequences = %#v, want 1 and 2", seen)
	}
}

func TestActiveWorkerCountHoldsSlotForDoneAndMergingChangeTasks(t *testing.T) {
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
	for _, state := range []State{StateDone, StateMerging} {
		id, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Change " + string(state), ShortName: "change-" + string(state)}, func(int) (bool, error) { return false, nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transition(ctx, id, StateSpawning, StateWorking, "cli", "started"); err != nil {
			t.Fatal(err)
		}
		if err := db.Transition(ctx, id, StateWorking, StateDone, "worker", "done"); err != nil {
			t.Fatal(err)
		}
		if state == StateMerging {
			if err := db.Transition(ctx, id, StateDone, StateMerging, "cli", "merging"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if count, err := db.ActiveWorkerCount(ctx, project.ID); err != nil || count != 2 {
		t.Fatalf("done and merging Change Tasks must hold slots: count=%d err=%v", count, err)
	}
}
