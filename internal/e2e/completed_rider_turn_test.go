//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Reproduce #279 with a Rider-like foreground process: it signals done, writes
// its closing reply, and remains working until its turn ends.
func TestDoneRiderClosingReplyDoesNotReopenTask(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	agent := `#!/bin/sh
set -eu
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    launch_path=${prompt#Read }
    launch_path=${launch_path% and follow it.}
    task_id=$(basename "$(dirname "$launch_path")")
    printf 'worker change %s\n' "$task_id" > "e2e-worker-$task_id.txt"
    git add "e2e-worker-$task_id.txt"
    git commit -m "worker change $task_id" >/dev/null
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    : > "$POSSE_TEST_ROOT/worker-ready-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/$task_id" ]; do sleep 0.02; done
    case "$task_id" in t1) pr_url='https://github.com/acme/shop/pull/17' ;; *) exit 0 ;; esac
    "$POSSE_E2E_POSSE_BIN" publish "Worker committed $task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    "$POSSE_E2E_POSSE_BIN" holler done "Worker committed $task_id" --pr "$pr_url" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    printf 'closing reply\n' > "$POSSE_TEST_ROOT/closing-reply-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/close-turn-$task_id" ]; do sleep 0.02; done
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    : > "$POSSE_TEST_ROOT/turn-ended-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/new-turn-$task_id" ]; do sleep 0.02; done
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
	brief := filepath.Join(fixture.root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Completed Rider turn\ndone_when: committed change exists\n---\nCreate a committed change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", "completed-rider-turn"); !strings.Contains(output, "t1") {
		t.Fatalf("ride did not return t1: %s", output)
	}
	fixture.waitTaskState(t, "t1", store.StateWorking)
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fixture.root, "worker-ready-t1"))
		return err == nil
	}) {
		t.Fatal("Rider did not reach its pre-Signal turn")
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "t1"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	closingReply := filepath.Join(fixture.root, "closing-reply-t1")
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(closingReply)
		return err == nil
	}) {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("Rider did not write its post-Signal closing reply: %s", log)
	}

	// Reconcile after the old five-second grace while the same Rider turn is
	// still active, matching the watcher race seen after the done Signal.
	time.Sleep(6 * time.Second)
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	stateAfterClosingReply := fixture.mustTask(t, "t1").State
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "close-turn-t1"), []byte("end\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fixture.root, "turn-ended-t1"))
		return err == nil
	}) {
		t.Fatal("Rider did not report the end of its turn")
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	stateAfterTurnEnd := fixture.mustTask(t, "t1").State

	// A subsequent working event represents new input after the turn boundary
	// and must reopen the completed Task.
	if _, err := herdr.NewWithEnv("herdr", fixture.env).Run(context.Background(), "pane", "report-agent", fixture.mustTask(t, "t1").PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatalf("report new Rider turn: %v", err)
	}
	if !waitForCondition(12*time.Second, func() bool {
		return fixture.mustTask(t, "t1").State == store.StateWorking
	}) {
		t.Fatalf("new Rider turn did not reopen Task: %#v", fixture.mustTask(t, "t1"))
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "new-turn-t1"), []byte("finish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stateAfterClosingReply != store.StateDone || stateAfterTurnEnd != store.StateDone {
		t.Fatalf("same-turn closing reply reopened done Task: after reply=%s after turn end=%s; want done", stateAfterClosingReply, stateAfterTurnEnd)
	}
}
