//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// queuedAdmissionFixture holds t1 in the only Worker slot and queues t2 behind
// it. No hidden Project watch runs, so only the test's commands admit t2.
type queuedAdmissionFixture struct {
	*workerTabsFixture
}

func newQueuedAdmissionFixture(t *testing.T) queuedAdmissionFixture {
	t.Helper()
	return queueBehindSlot(t, newWorkerTabsFixtureWithManualWatchOwner(t))
}

// queueBehindSlot fills the only Worker slot of f with t1 and queues t2.
func queueBehindSlot(t *testing.T, f *workerTabsFixture) queuedAdmissionFixture {
	t.Helper()
	config := "[lead]\nkind = \"claude\"\n\n[defaults]\nmerge_mode = \"local\"\nauto_teardown = \"never\"\nmax_workers = 1\n\n[profiles.deep]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"deep\"\n"
	if err := os.WriteFile(filepath.Join(f.home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	f.start(t, "t1", "Holds the slot", "holds-slot")
	brief := filepath.Join(f.root, "queued.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Queued worker\ndone_when: the test tears it down\n---\nWait.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "start", "--brief", brief, "--name", "queued-worker", "--queue"); !strings.Contains(output, "queued") {
		t.Fatalf("start --queue did not queue: %s", output)
	}
	f.waitState(t, "t2", store.StateQueued)
	return queuedAdmissionFixture{f}
}

// freeSlot tears t1 down, which makes room for the queued t2.
func (f queuedAdmissionFixture) freeSlot(t *testing.T) {
	t.Helper()
	f.fail(t, "t1")
	f.exitWorker(t, "t1")
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1", "--discard", "--user-approved", "User approved the queued admission test")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("teardown did not free the slot: %s", output)
	}
	if task := f.task(t, "t2"); task.State != store.StateQueued {
		t.Fatalf("t2 left the queue before any command reconciled: %#v", task.State)
	}
}

func (f queuedAdmissionFixture) heldSandboxes(t *testing.T, taskID int64) int {
	t.Helper()
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sandboxes, err := db.Sandboxes(context.Background(), f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, sandbox := range sandboxes {
		if sandbox.TaskID == taskID && sandbox.State == "held" {
			held++
		}
	}
	return held
}

// TestQueuedTaskAdmittedAtFreedSlotStarts queues a Task at capacity, frees the
// slot by Teardown and expects the next command to admit and start it.
func TestQueuedTaskAdmittedAtFreedSlotStarts(t *testing.T) {
	f := newQueuedAdmissionFixture(t)
	f.freeSlot(t)
	runPosse(t, f.binary, f.repo, f.leadEnv, "list")
	f.waitState(t, "t2", store.StateWorking)
	if task := f.task(t, "t2"); task.Launches != 1 || task.PaneID == "" {
		t.Fatalf("admitted Task did not launch its Worker: %#v", task)
	}
}

// TestQueuedTaskAdmittedByOneWatchPoll frees the slot and runs no reconciling
// command; a single Project watch poll must admit and start the queued Task.
func TestQueuedTaskAdmittedByOneWatchPoll(t *testing.T) {
	f := newQueuedAdmissionFixture(t)
	f.freeSlot(t)
	runPosse(t, f.binary, f.repo, setEnv(f.leadEnv, "POSSE_WATCH_TOKEN", ""), "watch", "--poll-only", "--timeout", "1")
	if task := f.task(t, "t2"); task.State == store.StateQueued {
		t.Fatalf("one watch poll left t2 queued with a free Worker slot: %#v", task.State)
	}
	f.waitState(t, "t2", store.StateWorking)
}

// TestHeldProjectWatchPollNamesTheGenerationBlocker records a Herdr generation
// the live server no longer has, as a missed `posse up --all` leaves it, with a
// block episode already three minutes old. The held Project stays inert per
// ADR 0035, but one watch poll raises one Notice naming the cause, and the
// queued Task's wait_reason shows it.
func TestHeldProjectWatchPollNamesTheGenerationBlocker(t *testing.T) {
	f := newQueuedAdmissionFixture(t)
	f.freeSlot(t)
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.SetProjectServerStartedAt(ctx, f.projectID, "1:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordReconcileBlock(ctx, f.projectID, "earlier pass", time.Now().Add(-3*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	watchEnv := setEnv(f.leadEnv, "POSSE_WATCH_TOKEN", "")
	for range 2 {
		runPosse(t, f.binary, f.repo, watchEnv, "watch", "--poll-only", "--timeout", "1")
	}
	if task := f.task(t, "t2"); task.State != store.StateQueued {
		t.Fatalf("a held Project admitted t2: %#v", task.State)
	}
	notices, err := db.Notices(ctx, f.projectID, false)
	if err != nil {
		t.Fatal(err)
	}
	var blocked []string
	for _, notice := range notices {
		if notice.Kind == "reconcile_blocked" {
			blocked = append(blocked, notice.Summary)
		}
	}
	if len(blocked) != 1 || !strings.Contains(blocked[0], "replaced the recorded 1:1; run `posse up --all`") {
		t.Fatalf("held watch polls raised reconcile_blocked Notices %q, want one naming the generation", blocked)
	}
	show := runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t2")
	if !strings.Contains(show, "waiting for Project reconciliation") || !strings.Contains(show, "posse up --all") {
		t.Fatalf("held wait_reason does not name the blocker: %s", show)
	}
}

// TestQueuedTaskStartSurvivesKilledCaller kills the foreground command that
// admitted t2 while the start is under way. The start runs detached, so t2
// still reaches working.
func TestQueuedTaskStartSurvivesKilledCaller(t *testing.T) {
	f := newQueuedAdmissionFixture(t)
	f.freeSlot(t)
	marker := filepath.Join(f.root, "queued-start-paused")
	caller := exec.Command(f.binary, "list")
	caller.Dir = f.repo
	caller.Env = setEnv(setEnv(f.leadEnv, "POSSE_INTENT_PAUSE_AT", "start:after:pane.record"), "POSSE_INTENT_PAUSE_FILE", marker)
	caller.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	caller.Stdout, caller.Stderr = &output, &output
	if err := caller.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(30*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
		t.Fatalf("queued start never reached its pause: %s", output.String())
	}
	if task := f.task(t, "t2"); task.State != store.StateSpawning || task.PaneID == "" {
		t.Fatalf("t2 was not mid-start: %#v", task)
	}
	// The User interrupting the Lead kills the foreground command's group.
	if err := syscall.Kill(-caller.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = caller.Wait()
	if err := os.WriteFile(marker+".continue", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.waitState(t, "t2", store.StateWorking)
	if task := f.task(t, "t2"); task.Launches != 1 {
		t.Fatalf("detached start did not launch the Worker: %#v", task)
	}
}

// TestInterruptedQueuedStartIsCleanedUp crashes the start right after Posse
// opened t2's pane, which then holds only its own shell. Recovery fails t2 with
// a visible reason, closes that pane and releases the Sandbox.
func TestInterruptedQueuedStartIsCleanedUp(t *testing.T) {
	f := newQueuedAdmissionFixture(t)
	f.freeSlot(t)
	crash := exec.Command(f.binary, "list")
	crash.Dir, crash.Env = f.repo, setEnv(f.leadEnv, "POSSE_INTENT_CRASH_AT", "start:after:pane.record")
	_, _ = crash.CombinedOutput()
	list := runPosse(t, f.binary, f.repo, f.leadEnv, "list")
	f.waitState(t, "t2", store.StateFailed)
	crashed := f.task(t, "t2")
	if crashed.PaneID == "" || crashed.Launches != 0 {
		t.Fatalf("start did not crash after opening its pane: %#v", crashed)
	}
	for _, unwanted := range []string{"cleanup failed", "release failed", "shutdown failed"} {
		if strings.Contains(list, unwanted) || strings.Contains(runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t2"), unwanted) {
			t.Fatalf("cleanup of the shell-only pane failed (%q): %s", unwanted, list)
		}
	}
	show := runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t2")
	if !strings.Contains(show, "reason: start was interrupted before its Worker started") {
		t.Fatalf("show omitted the recorded failure reason: %s", show)
	}
	for _, pane := range f.snapshot(t).Panes {
		if pane.PaneID == crashed.PaneID {
			t.Fatalf("pane %s Posse opened is still open", pane.PaneID)
		}
	}
	if held := f.heldSandboxes(t, crashed.ID); held != 0 {
		t.Fatalf("failed start still holds %d Sandbox(es)", held)
	}
}

// TestShowFullRendersBriefInputsAsText expects frozen Brief inputs as readable
// text, not as a decimal byte array.
func TestShowFullRendersBriefInputsAsText(t *testing.T) {
	f := newQueuedAdmissionFixture(t)
	show := runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t2", "--full")
	if !strings.Contains(show, "title: Queued worker") || strings.Contains(show, "bytes[") {
		t.Fatalf("show --full did not render the Brief input as text: %s", show)
	}
}
