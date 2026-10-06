//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutAckCommitsBeforeUnrelatedTeardown(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()

	ctx := context.Background()
	maintenanceTaskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 90, Type: "ship", Title: "Unrelated landed Task", LandingMode: "local",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
		note     string
	}{
		{store.StateSpawning, store.StateWorking, "cli", "started"},
		{store.StateWorking, store.StateDone, "worker", "finished"},
		{store.StateDone, store.StateLanding, "cli", "ready to land"},
		{store.StateLanding, store.StateLanded, "cli", "landed"},
	} {
		if err := f.db.Transition(ctx, maintenanceTaskID, transition.from, transition.to, transition.source, transition.note); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the unrelated automatic Teardown active so shared Project preparation
	// reaches its retryable teardown_in_progress failure before acknowledging.
	if err := f.db.StartIntent(ctx, f.project.ID, maintenanceTaskID, "unsaddle", "in_progress:panes.close", `{"discard":false}`, os.Getpid()); err != nil {
		t.Fatalf("start unrelated automatic Teardown: %v", err)
	}
	intent, err := f.db.IntentByTask(ctx, maintenanceTaskID)
	if err != nil {
		t.Fatal(err)
	}

	requestedID, err := f.db.CreateNotice(ctx, store.Notice{
		ProjectID: f.project.ID, Kind: "worker_blocked", Summary: "requested acknowledgement", DataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelatedID, err := f.db.CreateNotice(ctx, store.Notice{
		ProjectID: f.project.ID, Kind: "needs_decision", Summary: "leave this Notice open", DataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkNoticesDelivered(ctx, f.project.ID, []int64{requestedID, unrelatedID}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(requestedID, 10), "--timeout", "30000")
	command.Dir, command.Env = f.repo, f.leadEnv
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	commandRunning := true
	defer func() {
		if commandRunning {
			_ = command.Process.Kill()
			<-done
		}
	}()

	if !waitForCondition(2*time.Second, func() bool {
		return noticeAckedAt(t, f.db, f.project.ID, requestedID) != 0
	}) {
		select {
		case err := <-done:
			commandRunning = false
			t.Fatalf("Lookout exited before committing the requested acknowledgement: %v", err)
		default:
			t.Fatal("Lookout did not commit the requested acknowledgement before unrelated Teardown maintenance")
		}
	}
	if noticeAckedAt(t, f.db, f.project.ID, unrelatedID) != 0 {
		t.Fatal("Lookout acknowledged a Notice that was not requested")
	}
	select {
	case err := <-done:
		commandRunning = false
		t.Fatalf("Lookout exited instead of remaining armed after its acknowledgement: %v", err)
	default:
	}

	ackedAt := noticeAckedAt(t, f.db, f.project.ID, requestedID)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	commandRunning = false
	if got := noticeAckedAt(t, f.db, f.project.ID, requestedID); got != ackedAt {
		t.Fatalf("acknowledgement changed after interrupt: before=%d after=%d", ackedAt, got)
	}

	retry := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(requestedID, 10), "--timeout", "15000")
	retry.Dir, retry.Env = f.repo, f.leadEnv
	output, err := retry.CombinedOutput()
	if err != nil {
		t.Fatalf("retry acknowledged Notice and report unrelated maintenance failure: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Lookout maintenance failed (retryable)") {
		t.Fatalf("unrelated Teardown failure was not surfaced as a retryable Notice: %s", output)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var maintenanceNotice *store.Notice
	for i := range notices {
		if notices[i].Kind == "pr_watch_failing" && strings.Contains(notices[i].Summary, "Lookout maintenance failed (retryable)") {
			maintenanceNotice = &notices[i]
			break
		}
	}
	if maintenanceNotice == nil || maintenanceNotice.DeliveredAt == 0 || maintenanceNotice.AckedAt != 0 {
		t.Fatalf("maintenance failure Notice state = %#v, want delivered and still open", maintenanceNotice)
	}
	if err := f.db.FinishIntent(ctx, intent.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	finalRetry := exec.Command(f.binary, "lookout", "--ack", strconv.FormatInt(requestedID, 10), "--timeout", "100")
	finalRetry.Dir, finalRetry.Env = f.repo, f.leadEnv
	output, err = finalRetry.CombinedOutput()
	if err != nil {
		t.Fatalf("re-arm Lookout after repairing unrelated Teardown: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "state: timeout") {
		t.Fatalf("re-armed Lookout did not enter its wait loop: %s", output)
	}
	if got := noticeAckedAt(t, f.db, f.project.ID, requestedID); got != ackedAt {
		t.Fatalf("retry changed the successful acknowledgement: before=%d after=%d", ackedAt, got)
	}
	if noticeAckedAt(t, f.db, f.project.ID, unrelatedID) != 0 {
		t.Fatal("retry acknowledged a Notice that was not requested")
	}
	maintenanceTask, err := f.db.TaskByID(ctx, f.project.ID, maintenanceTaskID)
	if err != nil || maintenanceTask.State != store.StateTornDown {
		t.Fatalf("unrelated automatic Teardown was not retried after its owner finished: task=%#v err=%v", maintenanceTask, err)
	}
}

func noticeAckedAt(t *testing.T, db *store.DB, projectID, noticeID int64) int64 {
	t.Helper()
	var ackedAt int64
	if err := db.QueryRowContext(context.Background(), `SELECT COALESCE(acked_at, 0) FROM notices WHERE project_id=? AND id=?`, projectID, noticeID).Scan(&ackedAt); err != nil {
		t.Fatal(err)
	}
	return ackedAt
}
