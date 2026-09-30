package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	id, err := db.CreateTask(context.Background(), project.ID, Task{Seq: 1, Type: "ship", Title: "Old", LandingMode: "local"})
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local", AutonomyReview: "ask", AutonomyLand: "ask"})
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
	if version != 21 {
		t.Fatalf("applied Goose migration version = %d, want 21", version)
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local"})
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
	if version != 21 {
		db.Close()
		t.Fatalf("initial Goose migration version = %d, want 21", version)
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
		t.Fatalf("opening a database at version 21 without migration 12: %v", err)
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
	if version != 21 {
		t.Fatalf("reopened Goose migration version = %d, want 21", version)
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Fix", ShortName: "worker-tree", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.TaskByID(ctx, project.ID, id)
	if err != nil || task.ShortName != "worker-tree" {
		t.Fatalf("Task short name = %q, %v", task.ShortName, err)
	}
	data, err := os.ReadFile(db.TaskSnapshotPath(project.Name, task.Seq))
	if err != nil || !strings.Contains(string(data), "short_name = \"worker-tree\"") {
		t.Fatalf("Task snapshot short name missing: %q, %v", data, err)
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "intent", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "ride", "mount.acquire", `{}`, 1001); err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "ride", "pane.open", `{}`, 1002); !errors.Is(err, ErrIntentOwned) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "identity", LandingMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StartIntent(ctx, project.ID, taskID, "ride", "started", `{}`, os.Getpid()); err != nil {
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

func TestUnlandedTeardownRequiresRecordedUserApproval(t *testing.T) {
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local"})
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
		t.Fatal("unlanded Task was torn down without an approval record")
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local"})
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
	id, err := db.CreateTask(ctx, project.ID, Task{Seq: 5, Type: "ship", Title: "Fix", LandingMode: "local", PaneID: "w2:p5", PaneLabel: "posse:shop:t5"})
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
	if err := db.Transition(ctx, id, StateDone, StateLanding, "cli", "gate passed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateLanding, StateLanded, "cli", "landed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, StateLanded, StateTornDown, "cli", "released"); err != nil {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Fix", LandingMode: "local"})
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
			claimed, err := db.ClaimLeadStart(ctx, project.ID, now, now-2*time.Minute.Milliseconds())
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
	claimed, err := second.ClaimLeadStart(ctx, project.ID, now+3*time.Minute.Milliseconds(), now+time.Minute.Milliseconds())
	if err != nil || !claimed {
		t.Fatalf("stale Lead start claim was not recovered = %t, %v", claimed, err)
	}
	claimed, err = first.ClaimLeadStart(ctx, project.ID, now+3*time.Minute.Milliseconds()+1, now+time.Minute.Milliseconds())
	if err != nil || claimed {
		t.Fatalf("fresh Lead start claim allowed a second owner = %t, %v", claimed, err)
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
	if _, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "ship", ShortName: "same-name"}, occupied); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "ship", ShortName: "same-name"}, occupied); !errors.Is(err, ErrTaskBranchExists) {
		t.Fatalf("reused Task branch error = %v", err)
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil || len(tasks) != 1 || tasks[0].Branch != "posse/same-name" {
		t.Fatalf("Tasks after collision = %#v, %v", tasks, err)
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
			_, sequence, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "ship", Title: fmt.Sprintf("Concurrent %d", index), ShortName: fmt.Sprintf("concurrent-%d", index)}, func(int) (bool, error) {
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
