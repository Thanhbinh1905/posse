package app

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConcurrentCLIReportsTeardownInProgressInsteadOfMerged(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateMerging, "cli", "PR ready"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateMerging, store.StateMerged, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.StartIntent(ctx, f.project.ID, f.task.ID, "teardown", "sandbox.stop", "{}", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	intent, err := f.db.IntentByTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	limited, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	if err := waitForActiveTeardowns(limited, f.db, f.project.ID); err == nil || !strings.Contains(err.Error(), "Teardown is in progress") {
		t.Fatalf("half-torn-down Task was presented as merged: %v", err)
	}
	if err := f.db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
		t.Fatal(err)
	}
	if err := waitForActiveTeardowns(ctx, f.db, f.project.ID); err != nil {
		t.Fatal(err)
	}
}

func TestWatchMaintenanceTreatsActiveTeardownAsRoutine(t *testing.T) {
	f := newPRMergeFixture(t, "pr", store.StateDone)
	defer f.db.Close()
	ctx := context.Background()
	if err := f.db.Transition(ctx, f.task.ID, store.StateDone, store.StateMerging, "cli", "PR ready"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, f.task.ID, store.StateMerging, store.StateMerged, "cli", "merged"); err != nil {
		t.Fatal(err)
	}
	owner := exec.Command("sleep", "30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill(); _ = owner.Wait() }()
	if err := f.db.StartIntent(ctx, f.project.ID, f.task.ID, "teardown", "sandbox.stop", "{}", owner.Process.Pid); err != nil {
		t.Fatal(err)
	}
	previous := teardownWaitTimeout
	teardownWaitTimeout = 200 * time.Millisecond
	defer func() { teardownWaitTimeout = previous }()
	if err := f.service.maintainProjectWatch(ctx, f.db, f.project); err != nil {
		t.Fatalf("a Teardown in progress failed Watch maintenance: %v", err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" {
			t.Fatalf("a Teardown in progress raised %#v", notice)
		}
	}
}

func TestTeardownCompletesSettledIntentOfLiveOwner(t *testing.T) {
	fixture, task, cfg := newMergedLocalTeardownFixture(t)
	ctx := context.Background()
	// The owner reached torn-down after this caller read the Task as merged,
	// then never finished its intent while its process stayed alive.
	owner := exec.Command("sleep", "30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill(); _ = owner.Wait() }()
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, task.ID, "teardown", "done:task.torn_down", `{"discard":false}`, owner.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, task.ID, store.StateMerged, store.StateTornDown, "cli", "Task Sandbox released after completion"); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.teardownTask(ctx, fixture.db, fixture.project, cfg, task, false, "")
	if err != nil || !result.AlreadyTornDown {
		t.Fatalf("Teardown of a settled intent = %#v, %v", result, err)
	}
	if intent, err := fixture.db.IntentByTask(ctx, task.ID); !store.IsNotFound(err) {
		t.Fatalf("settled Teardown intent stayed active: %#v, %v", intent, err)
	}
}

func TestTeardownCommandCompletesSettledIntentOfTornDownTask(t *testing.T) {
	fixture, task, _ := newMergedLocalTeardownFixture(t)
	ctx := context.Background()
	owner := exec.Command("sleep", "30")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill(); _ = owner.Wait() }()
	if err := fixture.db.StartIntent(ctx, fixture.project.ID, task.ID, "teardown", "done:task.torn_down", `{"discard":false}`, owner.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Transition(ctx, task.ID, store.StateMerged, store.StateTornDown, "cli", "Task Sandbox released after completion"); err != nil {
		t.Fatal(err)
	}
	if code, output, stderr := fixture.run("teardown", "t1"); code != 0 || !strings.Contains(output, "already_torn_down") {
		t.Fatalf("teardown of a torn-down Task = %d %s %s", code, output, stderr)
	}
	if intent, err := fixture.db.IntentByTask(ctx, task.ID); !store.IsNotFound(err) {
		t.Fatalf("settled Teardown intent stayed active: %#v, %v", intent, err)
	}
}
