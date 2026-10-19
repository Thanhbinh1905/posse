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

// Reproduce #279 and a direct User follow-up: the closing reply stays done,
// then a new turn signals working, publishes and completes again.
func TestDoneWorkerClosingReplyDoesNotReopenTask(t *testing.T) {
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
    "$POSSE_E2E_POSSE_BIN" signal done "Worker committed $task_id" --pr "$pr_url" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    printf 'closing reply\n' > "$POSSE_TEST_ROOT/closing-reply-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/close-turn-$task_id" ]; do sleep 0.02; done
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    : > "$POSSE_TEST_ROOT/turn-ended-$task_id"
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/new-turn-$task_id" ]; do sleep 0.02; done
    IFS= read -r request || exit 0
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    printf 'signal working %s\\n' "$request" >> "$POSSE_TEST_ROOT/worker-delivery.log"
    "$POSSE_E2E_POSSE_BIN" signal working "$request" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    : > "$POSSE_TEST_ROOT/follow-up-working-$task_id"
    printf 'direct User follow-up\n' > "follow-up-$task_id.txt"
    git add "follow-up-$task_id.txt"
    git commit -m "direct User follow-up $task_id" >/dev/null
    printf 'publish Published direct User follow-up %s\\n' "$task_id" >> "$POSSE_TEST_ROOT/worker-delivery.log"
    "$POSSE_E2E_POSSE_BIN" publish "Published direct User follow-up $task_id" --verify "focused E2E -> passed" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    "$POSSE_E2E_POSSE_BIN" signal done "Completed direct User follow-up $task_id" --pr "$pr_url" >> "$POSSE_TEST_ROOT/worker-delivery.log" 2>&1
    : > "$POSSE_TEST_ROOT/follow-up-done-$task_id"
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
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Completed Worker turn\ndone_when: committed change exists\n---\nCreate a committed change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "completed-worker-turn"); !strings.Contains(output, "t1") {
		t.Fatalf("start did not return t1: %s", output)
	}
	fixture.waitTaskState(t, "t1", store.StateWorking)
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(fixture.root, "worker-ready-t1"))
		return err == nil
	}) {
		t.Fatal("Worker did not reach its pre-Signal turn")
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
		t.Fatalf("Worker did not write its post-Signal closing reply: %s", log)
	}

	// Reconcile after the old five-second grace while the same Worker turn is
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
		t.Fatal("Worker did not report the end of its turn")
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	stateAfterTurnEnd := fixture.mustTask(t, "t1").State

	// A direct User request starts a fresh Worker turn. The Worker explicitly
	// reopens its Task before changing and publishing the branch.
	client := herdr.NewWithEnv("herdr", fixture.env)
	if _, err := client.Run(context.Background(), "pane", "run", fixture.mustTask(t, "t1").PaneID, "The User asked me to make one more change."); err != nil {
		t.Fatalf("send direct User follow-up to Worker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "new-turn-t1"), []byte("continue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workingMarker := filepath.Join(fixture.root, "follow-up-working-t1")
	if !waitForCondition(15*time.Second, func() bool {
		_, err := os.Stat(workingMarker)
		return err == nil
	}) {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("Worker did not record its working Signal: %s", log)
	}
	if task := fixture.mustTask(t, "t1"); task.State != store.StateWorking {
		t.Fatalf("direct User follow-up did not reopen Task before publishing: %#v", task)
	}
	doneMarker := filepath.Join(fixture.root, "follow-up-done-t1")
	if !waitForCondition(30*time.Second, func() bool {
		_, err := os.Stat(doneMarker)
		return err == nil
	}) {
		log, _ := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
		t.Fatalf("Worker did not publish and complete the direct User follow-up: %s", log)
	}
	fixture.waitTaskState(t, "t1", store.StateDone)
	final := fixture.mustTask(t, "t1")
	if final.PRURL != "https://github.com/acme/shop/pull/17" {
		t.Fatalf("follow-up completion lost the published PR: %#v", final)
	}
	publishedSHA := strings.TrimSpace(gitTest(t, fixture.env, fixture.remote, "rev-parse", "refs/heads/posse/t1"))
	worktreeSHA := strings.TrimSpace(gitTest(t, fixture.env, final.WorktreePath, "rev-parse", "HEAD"))
	if publishedSHA != worktreeSHA {
		t.Fatalf("direct User follow-up was not published: remote=%s Worker=%s", publishedSHA, worktreeSHA)
	}
	transitions, err := fixture.db.TaskTransitions(context.Background(), final.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	resumed := false
	for _, transition := range transitions {
		if transition.To == string(store.StateWorking) && transition.Source == "worker" && transition.Note == "The User asked me to make one more change." {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("direct User follow-up lacks a reasoned Worker resume transition: %#v", transitions)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	notified := false
	for _, notice := range notices {
		if notice.TaskID == final.ID && notice.Kind == "worker_resumed" && strings.Contains(notice.Summary, `"The User asked me to make one more change."`) {
			notified = true
		}
	}
	if !notified {
		t.Fatalf("Lead was not notified with the direct follow-up reason: %#v", notices)
	}
	if stateAfterClosingReply != store.StateDone || stateAfterTurnEnd != store.StateDone {
		t.Fatalf("same-turn closing reply reopened done Task: after reply=%s after turn end=%s; want done", stateAfterClosingReply, stateAfterTurnEnd)
	}
	log, err := os.ReadFile(filepath.Join(fixture.root, "worker-delivery.log"))
	if err != nil || !strings.Contains(string(log), "signal working The User asked me to make one more change.") || !strings.Contains(string(log), "publish Published direct User follow-up t1") {
		t.Fatalf("follow-up Signal and publish were not recorded: %s err=%v", log, err)
	}
}
