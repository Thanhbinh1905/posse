package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReconcileErrorMapsTimeoutWithoutDiscardingOtherFailures(t *testing.T) {
	persistent := errors.New("reconcile Task t1: constraint failure")
	timeout := fmt.Errorf("reconcile Task t2: %w", context.DeadlineExceeded)

	got := reconcileError(errors.Join(persistent, timeout))
	if !store.IsBusy(got) || store.IsOnlyBusy(got) {
		t.Fatalf("reconcileError() = %v, want mixed retryable and persistent failures", got)
	}
	if errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("reconcileError() exposed a raw timeout: %v", got)
	}
	visible := store.WithoutBusy(got)
	if visible == nil || !strings.Contains(visible.Error(), "reconcile Task t1") || strings.Contains(visible.Error(), "t2") {
		t.Fatalf("non-contention failures = %v, want only the attributed t1 failure", visible)
	}

	deferred := reconcileError(timeout)
	if !store.IsOnlyBusy(deferred) || store.WithoutBusy(deferred) != nil {
		t.Fatalf("reconcileError() timeout = %v, want pure retryable deferral", deferred)
	}
}

func TestRunPreservesTaskFailureWhenAnotherTaskTimesOut(t *testing.T) {
	db, project, first := createWorkingTask(t)
	defer db.Close()

	second := store.Task{
		Seq: 2, Type: "ship", Title: "Second task", LandingMode: "local",
		WorktreePath: t.TempDir(), PaneID: "w1:p2", PaneLabel: "posse:shop:t2",
		HerdrWorkspaceID: "w1", AutonomyReview: "ask", AutonomyLand: "ask",
	}
	secondID, err := db.CreateTask(context.Background(), project.ID, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), secondID, store.StateSpawning, store.StateWorking, "cli", "agent ready"); err != nil {
		t.Fatal(err)
	}

	adapter := herdr.NewFake()
	adapter.SnapshotValue.Panes = []herdr.Pane{
		{PaneID: first.PaneID, WorkspaceID: first.HerdrWorkspaceID, Label: first.PaneLabel, Agent: "claude", AgentStatus: "working"},
		{PaneID: second.PaneID, WorkspaceID: second.HerdrWorkspaceID, Label: second.PaneLabel, Agent: "claude", AgentStatus: "working"},
	}
	progress := taskFailureProgress{
		failures:       map[string]error{first.WorktreePath: errors.New("synthetic persistent failure")},
		waitForTimeout: second.WorktreePath,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err = Run(ctx, db, adapter, project.ID, time.Minute, 0, time.Now(), progress)
	if err == nil {
		t.Fatal("Run() error = nil, want the persistent Task failure and timeout deferral")
	}
	if !store.IsBusy(err) {
		t.Fatalf("Run() error = %v, want the timed-out Task classified as retryable contention", err)
	}
	if store.IsOnlyBusy(err) {
		t.Fatalf("Run() error = %v, classified the whole pass as contention and hid the other Task failure", err)
	}
	visible := store.WithoutBusy(err)
	if visible == nil || !strings.Contains(visible.Error(), "read Rider worktree t1") || !strings.Contains(visible.Error(), "synthetic persistent failure") {
		t.Fatalf("visible failure = %v, want the attributed t1 failure", visible)
	}
	if !strings.Contains(err.Error(), "read Rider worktree t2") {
		t.Fatalf("Run() error = %v, want the timeout attributed to t2", err)
	}
}

type taskFailureProgress struct {
	failures       map[string]error
	waitForTimeout string
}

func (p taskFailureProgress) ReadPane(context.Context, herdr.Adapter, string, int) (string, error) {
	return "unchanged", nil
}

func (p taskFailureProgress) WorktreeFingerprint(ctx context.Context, path string, _ bool) (string, error) {
	if err := p.failures[path]; err != nil {
		return "", err
	}
	if path == p.waitForTimeout {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return fmt.Sprintf("fingerprint:%s", path), nil
}
