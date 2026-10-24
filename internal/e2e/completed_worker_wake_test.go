//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Reproduce #443: a wake turn after done that only acknowledges a result keeps
// the Task done, while a wake turn that changes HEAD reopens it and queues an
// instruction to signal again.
func TestDoneWorkerWakeTurnReopensOnlyWhenRepositoryChanges(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	agent := `#!/bin/sh
set -eu
report() { herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state "$1" >/dev/null 2>&1; }
report idle
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    launch_path=${prompt#Read }
    launch_path=${launch_path% and follow it.}
    task_id=$(basename "$(dirname "$launch_path")")
    printf 'worker change %s\n' "$task_id" > "e2e-worker-$task_id.txt"
    git add "e2e-worker-$task_id.txt"
    git commit -m "worker change $task_id" >/dev/null
    report working
    : > "$POSSE_TEST_ROOT/worker-ready-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/$task_id" ]; do sleep 0.02; done
    "$POSSE_E2E_POSSE_BIN" publish "Worker committed $task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    "$POSSE_E2E_POSSE_BIN" signal done "Worker committed $task_id" --pr https://github.com/acme/shop/pull/17 --turn launch:1 >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    report idle
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/wake-nochange-$task_id" ]; do sleep 0.02; done
    report working
    sleep 6
    report idle
    : > "$POSSE_TEST_ROOT/wake-nochange-done-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/wake-change-$task_id" ]; do sleep 0.02; done
    report working
    printf 'wake change\n' > "wake-$task_id.txt"
    git add "wake-$task_id.txt"
    git commit -m "wake change $task_id" >/dev/null
    sleep 2
    report idle
    : > "$POSSE_TEST_ROOT/wake-change-done-$task_id"
    while IFS= read -r instruction; do printf '%s\n' "$instruction" >> "$POSSE_TEST_ROOT/worker-delivery.log"; done
    ;;
  *)
    while IFS= read -r line; do printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/lead-prompts.log"; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), []byte(agent), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "change.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Wake turn\ndone_when: committed change exists\n---\nCreate a committed change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "wake-turn"); !strings.Contains(output, "t1") {
		t.Fatalf("start did not return t1: %s", output)
	}
	fixture.waitTaskState(t, "t1", store.StateWorking)
	waitMarker := func(name string) {
		t.Helper()
		if !waitForCondition(15*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(fixture.root, name))
			return err == nil
		}) {
			log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
			t.Fatalf("missing %s: %s", name, log)
		}
	}
	gate := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", name), []byte("go\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitMarker("worker-ready-t1")
	gate("t1")
	fixture.waitTaskState(t, "t1", store.StateDone)
	// Let the watcher record the done turn boundary.
	time.Sleep(3 * time.Second)

	gate("wake-nochange-t1")
	waitMarker("wake-nochange-done-t1")
	time.Sleep(3 * time.Second)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	if task := fixture.mustTask(t, "t1"); task.State != store.StateDone {
		t.Fatalf("no-change wake turn reopened Task: %s", task.State)
	}

	gate("wake-change-t1")
	waitMarker("wake-change-done-t1")
	fixture.waitTaskState(t, "t1", store.StateWorking)
	notices, err := fixture.db.Notices(t.Context(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, notice := range notices {
		if notice.Kind == "worker_resumed" && strings.Contains(notice.Summary, "requires a new done Signal") {
			found = true
		}
	}
	if !found {
		t.Fatalf("changed wake turn raised no worker_resumed Notice: %#v", notices)
	}
	if !waitForCondition(20*time.Second, func() bool {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		return strings.Contains(string(log), "signal done again")
	}) {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("Worker was not told to signal again: %s", log)
	}
}
