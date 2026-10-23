package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

func TestLocalFetchContentionDoesNotInventRootBehindNotice(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	owner, err := acquireRepositoryFetchLock(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.syncProjectRoot(context.Background(), fixture.db, fixture.project, cfg, true)
	if !store.IsBusy(err) {
		t.Fatalf("fetch lock contention = %+v, %v; want retryable contention", result, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "root_behind") != 0 {
		t.Fatalf("local contention invented a root-behind warning: %#v", notices)
	}
}

func TestPollOnlyWatchDefersFetchLockContentionWithoutNotice(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	project, err := fixture.db.ProjectByID(context.Background(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake := fixture.service.Herdr.(*herdr.Fake)
	paneID := "w1:p9"
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{
		PaneID: paneID, WorkspaceID: project.HerdrWorkspaceID, Label: watchTabLabel(project),
	})
	t.Setenv("HERDR_PANE_ID", paneID)

	owner, err := acquireRepositoryFetchLock(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	defer unix.Flock(int(owner.Fd()), unix.LOCK_UN)

	if code, output, errOutput := fixture.run("watch", "--poll-only", "--timeout", "1"); code != 0 {
		t.Fatalf("poll-only Watch exited during expected fetch contention: code=%d output=%s error=%s", code, output, errOutput)
	} else if !strings.Contains(errOutput, "deferred due to transient store contention") {
		t.Fatalf("poll-only Watch did not log deferred contention: %s", errOutput)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "pr_watch_failing") != 0 {
		t.Fatalf("transient fetch contention raised a Notice: %#v", notices)
	}
}

func TestWatchDefersPureContentionButNotMixedFailures(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	var diagnostics bytes.Buffer
	ctx := &axi.Context{Context: context.Background(), ErrOut: &diagnostics}
	lastFailures := map[string]string{}

	busy := errors.Join(fmt.Errorf("checkout sync: %w", store.ErrBusy), store.ErrBusy)
	recordWatchFailure(ctx, fixture.db, fixture.project.ID, lastFailures, "reconcile", busy)
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "pr_watch_failing") != 0 || !strings.Contains(diagnostics.String(), "deferred due to transient store contention") {
		t.Fatalf("pure contention was not logged and deferred without a Notice: notices=%#v diagnostics=%q", notices, diagnostics.String())
	}

	persistent := errors.New("Task t2 observation: constraint failed")
	mixed := errors.Join(fmt.Errorf("Task t1 observation: %w", store.ErrBusy), persistent)
	recordWatchFailure(ctx, fixture.db, fixture.project.ID, lastFailures, "reconcile", mixed)
	notices, err = fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "pr_watch_failing") != 1 {
		t.Fatalf("mixed contention and persistent failure did not create a Notice: %#v", notices)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" && (!strings.Contains(notice.Summary, persistent.Error()) || strings.Contains(notice.Summary, store.ErrBusy.Error())) {
			t.Fatalf("mixed failure Notice lost its persistent cause or exposed raw contention: %#v", notice)
		}
	}
}

func TestRepositorySyncSkipsAnOverlappingSameTarget(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	target := repoTarget{Root: fixture.repo, DefaultBranch: fixture.project.DefaultBranch}
	key := repositorySyncKey(fixture.project, target)
	if !fixture.service.beginMaintenance(key) {
		t.Fatal("could not reserve repository sync target")
	}
	defer fixture.service.endMaintenance(key)
	result, err := fixture.service.syncRepository(context.Background(), fixture.db, fixture.project, target, time.Now(), false)
	if err != nil || result.Status != "skipped" {
		t.Fatalf("overlapping repository sync = %+v, %v; want skipped", result, err)
	}
}

func TestPoolDisplaysResourceScopeAndOwner(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	other, err := fixture.db.CreateProject(context.Background(), "other", fixture.root, "main")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := fixture.db.AcquireResourceLease(context.Background(), store.ResourceLeaseRequest{
		ResourceType: store.ResourcePath, ScopePath: fixture.repo, OwnerProjectID: other.ID,
		Owner: "local root writer", SourceKey: "test-local-root-writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.db.ReleaseResourceLease(context.Background(), lease.ID, lease.CustodyToken) }()
	code, output, errOutput := fixture.run("pool")
	if code != 0 || !strings.Contains(output, fixture.repo) || !strings.Contains(output, "local root writer") {
		t.Fatalf("Pool resource status = %d, %s, %s; want scope and owner", code, output, errOutput)
	}
}

func TestPoolReconcilesAnExplicitlyApprovedDeadResourceOwner(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	lease, err := fixture.db.AcquireResourceLease(context.Background(), store.ResourceLeaseRequest{
		ResourceType: store.ResourcePath, ScopePath: fixture.repo, OwnerProjectID: fixture.project.ID,
		Owner: "interrupted root writer", SourceKey: "interrupted-root-writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.ExecContext(context.Background(), `UPDATE resource_leases SET owner_pid=99999999,owner_boot_id='old-boot',owner_start_time='old-start' WHERE id=?`, lease.ID); err != nil {
		t.Fatal(err)
	}
	code, output, errOutput := fixture.run("pool", "reconcile-resource", fmt.Sprint(lease.ID), "--user-approved", "verified the editor stopped and inspected the root")
	if code != 0 || !strings.Contains(output, "reconciled_resource_lease") {
		t.Fatalf("approved dead-owner reconciliation = %d, %s, %s", code, output, errOutput)
	}
	if _, err := fixture.db.ResourceLeaseByID(context.Background(), lease.ID); !store.IsNotFound(err) {
		t.Fatalf("reconciled lease still exists: err=%v", err)
	}
}

func TestPoolRefusesReconcileWhileResourceOwnerIsAlive(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	lease, err := fixture.db.AcquireResourceLease(context.Background(), store.ResourceLeaseRequest{
		ResourceType: store.ResourcePath, ScopePath: fixture.repo, OwnerProjectID: fixture.project.ID,
		Owner: "live root writer", SourceKey: "live-root-writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	code, output, errOutput := fixture.run("pool", "reconcile-resource", fmt.Sprint(lease.ID), "--user-approved", "inspected the root")
	if code == 0 || !strings.Contains(output+errOutput, "resource_owner_live") {
		t.Fatalf("live-owner reconciliation = %d, %s, %s; want refusal", code, output, errOutput)
	}
	if _, err := fixture.db.ResourceLeaseByID(context.Background(), lease.ID); err != nil {
		t.Fatalf("live owner's lease was removed: %v", err)
	}
}

func TestRepositorySyncSkipsWhenLocalRootWriterOwnsThePath(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	lease, err := fixture.db.AcquireResourceLease(context.Background(), store.ResourceLeaseRequest{
		ResourceType: store.ResourcePath, ScopePath: fixture.repo, OwnerProjectID: fixture.project.ID,
		Owner: "local root writer", SourceKey: "test-local-root-writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fixture.db.ReleaseResourceLease(context.Background(), lease.ID, lease.CustodyToken) }()
	target := repoTarget{Root: fixture.repo, DefaultBranch: fixture.project.DefaultBranch}
	result, err := fixture.service.syncRepository(context.Background(), fixture.db, fixture.project, target, time.Now(), false)
	if err != nil || result.Status != "skipped" || !strings.Contains(result.Reason, "local root writer") {
		t.Fatalf("sync with an overlapping root writer = %+v, %v; want a named lease refusal", result, err)
	}
}

func TestRepositorySyncSkipsAnInterprocessOverlap(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	owner, err := acquireRepositorySyncLock(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	defer unix.Flock(int(owner.Fd()), unix.LOCK_UN)
	target := repoTarget{Root: fixture.repo, DefaultBranch: fixture.project.DefaultBranch}
	// The holder is already syncing; a foreground command must not wait for it.
	started := time.Now()
	result, err := fixture.service.syncRepository(context.Background(), fixture.db, fixture.project, target, time.Now(), false)
	if err != nil || result.Status != "skipped" {
		t.Fatalf("interprocess repository overlap = %+v, %v; want skipped", result, err)
	}
	if elapsed := time.Since(started); elapsed >= fileLockWaitBudget/2 {
		t.Fatalf("interprocess repository overlap waited %s for the sync lock, want an immediate skip", elapsed)
	}
}

func TestForcedRepositorySyncWaitsForTheLockHolder(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	owner, err := acquireRepositorySyncLock(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// A merge's Git effects may hold the lock; the post-merge sync must follow it.
	const held = 200 * time.Millisecond
	go func() {
		time.Sleep(held)
		_ = unix.Flock(int(owner.Fd()), unix.LOCK_UN)
	}()
	target := repoTarget{Root: fixture.repo, DefaultBranch: fixture.project.DefaultBranch}
	started := time.Now()
	result, err := fixture.service.syncRepository(context.Background(), fixture.db, fixture.project, target, time.Now(), true)
	if err != nil || result.Reason == "repository sync already running" {
		t.Fatalf("forced repository sync = %+v, %v; want it to run after the holder releases the lock", result, err)
	}
	if elapsed := time.Since(started); elapsed < held {
		t.Fatalf("forced repository sync finished after %s, before the holder released the lock", elapsed)
	}
}

func TestMergeWaitsForRepositorySyncBeforeAcquiringGitResources(t *testing.T) {
	fixture := newPRMergeFixture(t, "local", store.StateDone)
	if err := fixture.db.Transition(context.Background(), fixture.task.ID, store.StateDone, store.StateMerging, "cli", "ready to merge"); err != nil {
		t.Fatal(err)
	}
	fixture.task.State = store.StateMerging
	if err := fixture.db.StartIntent(context.Background(), fixture.project.ID, fixture.task.ID, "merge", "merge", `{}`, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	intent, err := fixture.db.IntentByTask(context.Background(), fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := acquireRepositorySyncLock(context.Background(), fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	syncLease, err := fixture.db.AcquireResourceLease(context.Background(), store.ResourceLeaseRequest{
		ResourceType: store.ResourcePath, ScopePath: fixture.repo, OwnerProjectID: fixture.project.ID,
		Owner: "Posse sync " + fixture.project.Name, SourceKey: "test-repository-sync",
	})
	if err != nil {
		_ = owner.Close()
		t.Fatal(err)
	}

	mergeDone := make(chan error, 1)
	go func() {
		mergeDone <- withIntentGitResources(context.Background(), fixture.db, fixture.project, fixture.task, intent.ID, fixture.repo, []string{"refs/heads/main"}, fixture.repo, func() error { return nil })
	}()
	select {
	case err := <-mergeDone:
		_ = fixture.db.ReleaseResourceLease(context.Background(), syncLease.ID, syncLease.CustodyToken)
		_ = unix.Flock(int(owner.Fd()), unix.LOCK_UN)
		_ = owner.Close()
		t.Fatalf("merge tried to acquire conflicting resources before repository sync finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := fixture.db.ReleaseResourceLease(context.Background(), syncLease.ID, syncLease.CustodyToken); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-mergeDone:
		if err != nil {
			t.Fatalf("merge resource acquisition after repository sync = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("merge did not continue after repository sync released its lock")
	}
	if err := fixture.db.FinishIntent(context.Background(), intent.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
}

// One persistent configuration error fails both Watch phases on every pass.
// The alternating summaries must raise one Notice per phase, not one per pass.
func TestWatchFailureInBothPhasesRaisesOneNoticePerPhase(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	ctx := &axi.Context{Context: context.Background(), ErrOut: &bytes.Buffer{}}
	lastFailures := map[string]string{}
	persistent := errors.New("config.toml: defaults.review: legacy review policy")
	for range 5 {
		recordWatchFailure(ctx, fixture.db, fixture.project.ID, lastFailures, "reconcile", persistent)
		recordWatchFailure(ctx, fixture.db, fixture.project.ID, lastFailures, "watch", persistent)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := countNoticeKind(notices, "pr_watch_failing"); got != 2 {
		t.Fatalf("alternating Watch phase failures raised %d Notices, want 2: %#v", got, notices)
	}
}
