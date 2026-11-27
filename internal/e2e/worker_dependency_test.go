//go:build e2e && linux

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWorkerParksUntilDependencyLands(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	if err := os.WriteFile(filepath.Join(f.root, "quiet-agent"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	worker := f.start(t, "t1", "Blocked change", "blocked-change")
	dependency := f.start(t, "t2", "Fix the flaky check", "fix-flaky-check")
	workerEnv := workerSignalEnv(f, worker)
	dependencyEnv := workerSignalEnv(f, dependency)

	runPosse(t, f.binary, worker.WorktreePath, workerEnv, "signal", "blocked", "Waiting for the CI fix", "--on", "t2")
	if _, err := f.client.Run(t.Context(), "pane", "report-agent", worker.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Call(t.Context(), "pane.focus", map[string]any{"pane_id": f.leadPaneID}); err != nil {
		t.Fatal(err)
	}
	show := runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	if !strings.Contains(show, "t2:landed") || f.task(t, "t1").State != store.StateBlocked {
		t.Fatalf("Task did not stay blocked on t2 after reconciliation: %s", show)
	}
	runPosseExpectingFailure(t, f, worker.WorktreePath, workerEnv, "signal_state_invalid", "signal", "done", "Premature completion", "--no-change")
	runPosseExpectingFailure(t, f, worker.WorktreePath, workerEnv, "signal_state_invalid", "signal", "working", "Attempt to bypass the wait")
	runPosseExpectingFailure(t, f, f.repo, f.leadEnv, "worker_not_blocked", "keys", "t1", "enter")

	if err := os.WriteFile(filepath.Join(dependency.WorktreePath, "ci-fix.txt"), []byte("CI fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, dependency.WorktreePath, "add", "ci-fix.txt")
	gitTest(t, f.env, dependency.WorktreePath, "commit", "-m", "Fix the flaky check")
	runPosse(t, f.binary, dependency.WorktreePath, dependencyEnv, "signal", "done", "CI fix committed")
	runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t2")
	if task := f.task(t, "t1"); task.State != store.StateBlocked {
		t.Fatalf("a merge gate without landing unblocked t1: %s", task.State)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "merge", "t2", "--execute", "--user-approved", "User approved the CI fix merge")
	watchEnv := setEnv(f.leadEnv, "POSSE_WATCH_TOKEN", "")
	runPosse(t, f.binary, f.repo, watchEnv, "watch", "--poll-only", "--timeout", "1")
	resumed := f.task(t, "t1")
	if resumed.State != store.StateWorking || resumed.Launches != worker.Launches || resumed.WorktreePath != worker.WorktreePath || resumed.PaneID != worker.PaneID {
		t.Fatalf("landing did not resume the same Worker: %+v", resumed)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	messages, err := db.TaskMessagesAfter(t.Context(), worker.ID, 0, 10)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "Dependency t2 landed") {
		t.Fatalf("resume instructions = %+v, err = %v", messages, err)
	}
	decisions, err := db.PendingDecisions(t.Context(), f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range decisions {
		if decision.TaskID == worker.ID {
			t.Fatalf("parking raised a Decision: %+v", decision)
		}
	}
}

func TestWorkerDependencyRejectsInvalidTargetsAndCycles(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	if err := os.WriteFile(filepath.Join(f.root, "quiet-agent"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	one := f.start(t, "t1", "First dependant", "first-dependant")
	two := f.start(t, "t2", "Second dependant", "second-dependant")
	three := f.start(t, "t3", "Third dependant", "third-dependant")
	noChange := f.start(t, "t4", "Already fixed", "already-fixed")
	runPosse(t, f.binary, noChange.WorktreePath, workerSignalEnv(f, noChange), "signal", "done", "Already fixed on main", "--no-change")

	runPosseExpectingFailure(t, f, one.WorktreePath, workerSignalEnv(f, one), "cannot depend on itself", "signal", "blocked", "Wait", "--on", "t1")
	runPosseExpectingFailure(t, f, one.WorktreePath, workerSignalEnv(f, one), "does not exist in this Project", "signal", "blocked", "Wait", "--on", "t9")
	runPosseExpectingFailure(t, f, one.WorktreePath, workerSignalEnv(f, one), "cannot land", "signal", "blocked", "Wait", "--on", "t4")
	runPosse(t, f.binary, one.WorktreePath, workerSignalEnv(f, one), "signal", "blocked", "Wait", "--on", "t2")
	runPosse(t, f.binary, two.WorktreePath, workerSignalEnv(f, two), "signal", "blocked", "Wait", "--on", "t3")
	runPosseExpectingFailure(t, f, three.WorktreePath, workerSignalEnv(f, three), "would create a dependency cycle", "signal", "blocked", "Wait", "--on", "t1")
	if task := f.task(t, "t3"); task.State != store.StateWorking {
		t.Fatalf("invalid dependency changed t3's state: %s", task.State)
	}
}
