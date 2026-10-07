package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestHollerCommitsBeforeUnrelatedPreparationFailure(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET type='scout' WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.worktree, "report.md"), []byte("Review complete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	t.Setenv(workerHomeEnv, fixture.home)
	fixture.service.Herdr.(*herdr.Fake).Errors["session.snapshot"] = errors.New("unrelated snapshot failed")
	code, out, errOut := fixture.run("holler", "done", "Review complete", "--report", "report.md")
	if code != 0 {
		t.Fatalf("unrelated preparation rejected Signal: exit=%d output=%s stderr=%s", code, out, errOut)
	}
	updated, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil || updated.State != store.StateReported {
		t.Fatalf("Signal did not commit: state=%s error=%v", updated.State, err)
	}
}

func TestHollerOwnTransactionContentionIsRetryable(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	t.Setenv(workerHomeEnv, fixture.home)
	writer, err := fixture.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	err = fixture.service.signal(&axi.Context{Context: ctx, Out: &out, ErrOut: &errOut}, []string{"working", "Still working"})
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "store_busy" || !failure.Retryable {
		t.Fatalf("Signal contention = %v, want retryable store_busy", err)
	}
	task, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil || task.State != store.StateWorking {
		t.Fatalf("uncommitted Signal changed state: %s, %v", task.State, err)
	}
	signals, err := fixture.db.TaskSignals(context.Background(), fixture.task.ID, 10)
	if err != nil || len(signals) != 0 {
		t.Fatalf("contended Signal committed: %#v, %v", signals, err)
	}
}

func TestLookoutRemainsArmedDuringObservationContention(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	fake := fixture.service.Herdr.(*herdr.Fake)
	for i := range fake.SnapshotValue.Panes {
		if fake.SnapshotValue.Panes[i].PaneID != fixture.task.PaneID {
			fake.SnapshotValue.Panes[i].AgentStatus = "working"
		}
	}
	writer, err := fixture.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(context.Background(), `INSERT INTO notices(project_id,kind,summary,data_json,created_at) VALUES(?,'task_done','Writer released','{}',?)`, fixture.project.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() { time.Sleep(1500 * time.Millisecond); released <- writer.Commit() }()
	started := time.Now()
	code, out, errOut := fixture.run("lookout", "--timeout", "10000")
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if code != 0 || !bytes.Contains([]byte(out), []byte("Writer released")) {
		t.Fatalf("Lookout did not recover without restarting: exit=%d output=%s stderr=%s", code, out, errOut)
	}
	if time.Since(started) < 1500*time.Millisecond {
		t.Fatal("Lookout returned before writer release")
	}
}

func TestLookoutLogsAndRetriesMaintenanceContention(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	fake := fixture.service.Herdr.(*herdr.Fake)
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE tasks SET updated_at=1 WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	writer, err := fixture.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(context.Background(), `UPDATE projects SET lead_absent_since=lead_absent_since WHERE id=?`, fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		released <- writer.Commit()
	}()

	started := time.Now()
	code, out, errOut := fixture.run("lookout", "--timeout", "3000")
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("Lookout did not finish after writer release: exit=%d output=%s stderr=%s", code, out, errOut)
	}
	if !store.IsBusy(errors.New(errOut)) {
		t.Fatalf("Lookout did not expose its deferred contention: %s", errOut)
	}
	if fake.CallCount("session.snapshot") < 2 {
		t.Fatalf("Lookout did not retry reconciliation after contention: snapshots=%d", fake.CallCount("session.snapshot"))
	}
	updated, err := fixture.db.TaskByID(context.Background(), fixture.project.ID, fixture.task.ID)
	if err != nil || updated.UpdatedAt <= 1 {
		t.Fatalf("Lookout did not persist maintenance after writer release: updated_at=%d error=%v", updated.UpdatedAt, err)
	}
	if time.Since(started) < 1500*time.Millisecond {
		t.Fatal("Lookout returned before the contending writer released its lock")
	}
}

func TestRosterShowsDurableViewDuringObservationContention(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	writer, err := store.OpenAt(filepath.Join(fixture.home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	code, output, errOut := fixture.run("roster", "--json")
	if code != 0 || strings.Contains(output, "store_busy") || !strings.Contains(output, "tasks") || !strings.Contains(errOut, "Some Task observations were deferred") {
		t.Fatalf("roster did not show its durable view with a deferral note: exit=%d output=%s stderr=%s", code, output, errOut)
	}

	code, output, errOut = fixture.run("--json")
	var failure axi.Error
	if err := json.Unmarshal([]byte(output), &failure); err != nil {
		t.Fatalf("invalid dashboard failure: %s (stderr: %s): %v", output, errOut, err)
	}
	if code != 1 || failure.Code != "store_busy" || !failure.Retryable {
		t.Fatalf("dashboard observation contention failure = %#v, exit=%d; want retryable store_busy", failure, code)
	}
}

func TestHollerDoneSurvivesOtherTasksObservationContention(t *testing.T) {
	fixture := newPRLandingFixture(t, "local", store.StateWorking)
	ctx := context.Background()
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET type='scout', landing_mode='local', updated_at=1 WHERE id=?`, fixture.task.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.worktree, "report.md"), []byte("Task 1 report\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const taskCount = 256
	for seq := 2; seq <= taskCount; seq++ {
		id, err := fixture.db.CreateTask(ctx, fixture.project.ID, store.Task{
			Seq: seq, Type: "scout", Title: fmt.Sprintf("Review %d", seq), LandingMode: "local",
			WorktreePath: filepath.Join(fixture.root, fmt.Sprintf("task-%d", seq)), PaneID: "w3:p1",
			PaneLabel: fmt.Sprintf("posse:shop:t%d", seq), HerdrWorkspaceID: "w3",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET updated_at=1 WHERE project_id=?`, fixture.project.ID); err != nil {
		t.Fatal(err)
	}

	fake := fixture.service.Herdr.(*herdr.Fake)
	fake.SnapshotValue.ServerStartedAt = "current-generation"
	if err := fixture.db.SetProjectServerStartedAt(ctx, fixture.project.ID, fake.SnapshotValue.ServerStartedAt); err != nil {
		t.Fatal(err)
	}
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{
		PaneID: "w3:p1", WorkspaceID: "w3", Label: "posse:shop:t2", Agent: "claude", AgentStatus: "idle",
	})
	t.Setenv("HERDR_PANE_ID", fixture.task.PaneID)
	t.Setenv(workerHomeEnv, fixture.home)

	writer, err := store.OpenAt(filepath.Join(fixture.home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	writerStarted := make(chan struct{})
	releaseWriter := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWriter) }) }
	writerCtx, cancelWriter := context.WithTimeout(ctx, 20*time.Second)
	writerResult := make(chan error, 1)
	t.Cleanup(func() {
		release()
		cancelWriter()
		for err := range writerResult {
			if err != nil {
				t.Errorf("independent writer failed: %v", err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Errorf("close independent writer database: %v", err)
		}
	})
	go func() {
		defer close(writerResult)
		for {
			var updatedAt int64
			err := writer.QueryRowContext(writerCtx, `SELECT updated_at FROM tasks WHERE id=?`, fixture.task.ID).Scan(&updatedAt)
			if err != nil {
				writerResult <- err
				return
			}
			if updatedAt != 1 {
				break
			}
			if err := writerCtx.Err(); err != nil {
				writerResult <- fmt.Errorf("Task 1 observation did not commit before writer timeout: %w", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
		tx, err := writer.BeginTx(writerCtx, nil)
		if err != nil {
			writerResult <- err
			return
		}
		if _, err := tx.ExecContext(writerCtx, `UPDATE projects SET lead_absent_since=lead_absent_since WHERE id=?`, fixture.project.ID); err != nil {
			_ = tx.Rollback()
			writerResult <- err
			return
		}
		close(writerStarted)
		select {
		case <-releaseWriter:
		case <-writerCtx.Done():
		case <-time.After(7 * time.Second):
		}
		writerResult <- tx.Commit()
	}()

	type commandResult struct {
		code int
		out  string
		err  string
	}
	commandDone := make(chan commandResult, 1)
	go func() {
		code, out, errOut := fixture.run("holler", "done", "Review complete", "--report", "report.md")
		commandDone <- commandResult{code: code, out: out, err: errOut}
	}()
	select {
	case <-writerStarted:
	case err := <-writerResult:
		t.Fatalf("independent writer did not acquire its lock after Task 1 observation: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for independent writer to acquire the observation lock")
	}

	// The writer observes Task 1 changing before it blocks unrelated writes.
	// That change must already be the Signal, not a preparation observation.
	whileLocked, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil || whileLocked.State != store.StateReported {
		t.Errorf("Task 1 transition had not committed before unrelated reconciliation: state=%s err=%v", whileLocked.State, err)
	}
	result := <-commandDone
	release()
	if err := <-writerResult; err != nil {
		t.Fatalf("independent writer failed: %v", err)
	}
	if result.code != 0 {
		t.Fatalf("holler done failed during unrelated Task observation contention: exit=%d output=%s error=%s", result.code, result.out, result.err)
	}
	if store.IsBusy(errors.New(result.err)) {
		t.Fatalf("Rider saw a contention diagnostic after its Signal committed: %s", result.err)
	}

	updated, err := fixture.db.TaskByID(ctx, fixture.project.ID, fixture.task.ID)
	if err != nil || updated.State != store.StateReported {
		t.Fatalf("Task 1 Signal state = %q, %v; want reported", updated.State, err)
	}
	var lastObservationAt int64
	if err := fixture.db.QueryRowContext(ctx, `SELECT updated_at FROM tasks WHERE project_id=? AND seq=?`, fixture.project.ID, taskCount).Scan(&lastObservationAt); err != nil {
		t.Fatal(err)
	}
	if lastObservationAt != 1 {
		t.Fatal("reconcile overran its budget instead of deferring later observations")
	}
	t.Setenv(workerHomeEnv, "")
	t.Setenv("HERDR_PANE_ID", "")
	for pass := 0; pass < 8 && lastObservationAt <= 1; pass++ {
		if code, out, errOut := fixture.run("roster", "--json"); code != 0 {
			var failure axi.Error
			if err := json.Unmarshal([]byte(out), &failure); err != nil || failure.Code != "store_busy" || !failure.Retryable {
				t.Fatalf("deferred observations failed after release: exit=%d output=%s stderr=%s", code, out, errOut)
			}
		}
		if err := fixture.db.QueryRowContext(ctx, `SELECT updated_at FROM tasks WHERE project_id=? AND seq=?`, fixture.project.ID, taskCount).Scan(&lastObservationAt); err != nil {
			t.Fatal(err)
		}
	}
	if lastObservationAt <= 1 {
		t.Fatal("later observations did not progress on fresh bounded reconcile passes")
	}
}
