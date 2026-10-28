package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceMaintenanceAggregatesIndependentPhaseFailures(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend"}, []string{"backend"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fixture.service.maintainProjectWatch(ctx, fixture.db, fixture.project)
	if err == nil {
		t.Fatal("maintenance with a canceled context returned no phase failures")
	}
	for _, phase := range []string{"merged Task Teardown:", "PR polling:", "checkout sync:"} {
		if count := strings.Count(err.Error(), phase); count != 1 {
			t.Fatalf("maintenance error reported phase %q %d times: %v", phase, count, err)
		}
	}
}

func TestProjectShowIncludesMemberMaintenanceErrorsByPhase(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend", "worker"}, []string{"backend"})
	ctx := context.Background()
	for _, failure := range []struct {
		phase string
		text  string
	}{{memberPRPollPhase, "forge probe timed out"}, {memberCheckoutSyncPhase, "fetch did not finish"}} {
		if err := fixture.db.RecordMemberWatchError(ctx, fixture.project.ID, "backend", failure.phase, failure.text, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := fixture.service.projectRepoRows(ctx, fixture.db, fixture.project, fixture.config())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"Key":"name","Value":"backend"`, `"Key":"phase","Value":"pr_poll"`, `"Key":"phase","Value":"checkout_sync"`, "forge probe timed out", "fetch did not finish", `"Key":"name","Value":"worker"`} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("project repo status omits %q: %s", expected, data)
		}
	}
}

func TestMemberPollClaimIsReleasedAfterCallerDeadlineAndStoreContention(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", "working")
	ctx := context.Background()
	now := time.Now()
	token, err := fixture.db.ClaimMemberPRPoll(ctx, fixture.project.ID, "backend", now, time.Minute, true)
	if err != nil || token == "" {
		t.Fatalf("claim Member poll: token=%q err=%v", token, err)
	}
	writer, err := fixture.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET name=name WHERE id=?`, fixture.project.ID); err != nil {
		_ = writer.Rollback()
		t.Fatal(err)
	}

	callerCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- finishMemberPRPollOrRelease(callerCtx, fixture.db, fixture.project.ID, "backend", token, now.UnixMilli())
	}()
	time.Sleep(350 * time.Millisecond)
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("Member poll cleanup did not finish after SQLite contention cleared")
	}

	next, err := fixture.db.ClaimMemberPRPoll(ctx, fixture.project.ID, "backend", now.Add(time.Second), time.Minute, true)
	if err != nil || next == "" {
		t.Fatalf("Member poll claim remained stuck after its owner gave up: token=%q err=%v", next, err)
	}
	if err := fixture.db.ReleaseMemberPRPoll(ctx, fixture.project.ID, "backend", next); err != nil {
		t.Fatal(err)
	}
}

func TestMaintainProjectWatchStopsDetachedWorkBeforeReturningOnCanceledContext(t *testing.T) {
	fixture := newWorkspaceMergeFixture(t, localWorkspaceConfig, []string{"backend"}, []string{"backend"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = fixture.service.maintainProjectWatch(ctx, fixture.db, fixture.project)
	fixture.service.maintenanceMu.Lock()
	running := len(fixture.service.maintenance)
	fixture.service.maintenanceMu.Unlock()
	if running != 0 {
		t.Fatalf("%d maintenance goroutines still running after maintainProjectWatch returned on a canceled context", running)
	}
}
