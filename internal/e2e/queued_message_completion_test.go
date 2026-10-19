//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestQueuedMessageBlocksResearchDoneUntilInstructionApplied(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	fakeAgent := `#!/bin/sh
set -eu
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    launch_path=${prompt#Read }
    launch_path=${launch_path% and follow it.}
    printf 'Initial Research report.\n' > report.md
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/t1" ]; do sleep 0.02; done
    if "$POSSE_E2E_POSSE_BIN" signal done 'Isolated Research report ready' --report report.md >> "$POSSE_TEST_ROOT/research.log" 2>&1; then
      printf 'accepted\n' > "$POSSE_TEST_ROOT/first-done-result"
    else
      printf 'incomplete\n' > "$POSSE_TEST_ROOT/first-done-result"
    fi
    if [ -e "$POSSE_HOME/projects/shop/tasks/t1/report.md" ]; then : > "$POSSE_TEST_ROOT/first-report-copied"; fi
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    instruction=
    while IFS= read -r line; do
      printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/research.log"
      case "$line" in *'Please include the missing caveat before reporting done.'*) instruction=$line; break ;; esac
    done
    [ -n "$instruction" ] || exit 0
    printf 'Isolated Research report. Applied instruction: %s\n' "$instruction" > report.md
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    "$POSSE_E2E_POSSE_BIN" signal done 'Isolated Research report ready' --report report.md >> "$POSSE_TEST_ROOT/research.log" 2>&1
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    ;;
  *)
    while IFS= read -r line; do printf '%s\n' "$line" >> "$POSSE_TEST_ROOT/lead.log"; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), []byte(fakeAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "research.md")
	if err := os.WriteFile(brief, []byte("---\ntype: research\ntitle: Queued completion\ndone_when: report.md includes the queued caveat\n---\nWrite a concise Report.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "queued-completion"); !strings.Contains(output, "t1") {
		t.Fatalf("start did not return t1: %s", output)
	}
	if !waitForCondition(15*time.Second, func() bool {
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		return err == nil && task.State == store.StateWorking
	}) {
		task, _ := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		t.Fatalf("Research did not reach working: %#v", task)
	}

	const instruction = "Please include the missing caveat before reporting done."
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", instruction, "--queue")
	if !strings.Contains(output, "state: queued") {
		t.Fatalf("send did not queue the instruction: %s", output)
	}
	task := fixture.mustTask(t, "t1")
	message, err := fixture.db.OldestQueuedMessage(context.Background(), task.ID)
	if err != nil || message.Body != instruction {
		t.Fatalf("queued message = %#v, %v", message, err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "signal-gates", "t1"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(15*time.Second, func() bool {
		message, err := fixture.db.MessageByID(context.Background(), message.ID)
		return err == nil && message.Status == "delivered"
	}) {
		message, _ := fixture.db.MessageByID(context.Background(), message.ID)
		task, _ := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		log, _ := os.ReadFile(filepath.Join(fixture.root, "research.log"))
		t.Fatalf("queued instruction was not delivered after the incomplete done Signal: task=%#v message=%#v log=%s", task, message, log)
	}
	if !waitForCondition(15*time.Second, func() bool {
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		return err == nil && task.State == store.StateReported
	}) {
		task, _ := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		log, _ := os.ReadFile(filepath.Join(fixture.root, "research.log"))
		t.Fatalf("Research did not report after applying the instruction: task=%#v log=%s", task, log)
	}

	firstDone, err := os.ReadFile(filepath.Join(fixture.root, "first-done-result"))
	if err != nil || strings.TrimSpace(string(firstDone)) != "incomplete" {
		t.Fatalf("first done Signal result = %q, %v; want incomplete", firstDone, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "first-report-copied")); !os.IsNotExist(err) {
		t.Fatalf("incomplete done Signal copied its Report before retrying: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(fixture.root, "research.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "queued_instructions_pending") || !strings.Contains(string(log), "was not recorded") || !strings.Contains(string(log), instruction) {
		t.Fatalf("Worker did not see the incomplete Signal and queued instruction: %s", log)
	}
	message, err = fixture.db.MessageByID(context.Background(), message.ID)
	if err != nil || message.Status != "delivered" {
		t.Fatalf("queued message status after completion = %#v, %v; want delivered", message, err)
	}
	report, err := os.ReadFile(filepath.Join(fixture.home, "projects", "shop", "tasks", "t1", "report.md"))
	if err != nil || !strings.Contains(string(report), instruction) {
		t.Fatalf("saved Report does not apply the queued instruction: %q, %v", report, err)
	}
	signals, err := fixture.db.TaskSignals(context.Background(), task.ID, 10)
	if err != nil || len(signals) != 1 || signals[0].Verb != "done" {
		t.Fatalf("recorded Signals = %#v, %v; want only the final done Signal", signals, err)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == message.TaskID && notice.Kind == "queued_message_undeliverable" {
			t.Fatalf("queued message became undeliverable despite the Worker retrying done: %#v", notice)
		}
	}
}
