//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestQueuedMessageDoesNotStayQueuedWhenScoutReportsDone(t *testing.T) {
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
    printf 'Isolated Scout report.\n' > report.md
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/t1" ]; do sleep 0.02; done
    "$POSSE_E2E_POSSE_BIN" holler done 'Isolated Scout report ready' --report report.md >> "$POSSE_TEST_ROOT/scout.log" 2>&1
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    while IFS= read -r instruction; do printf '%s\\n' "$instruction" >> "$POSSE_TEST_ROOT/scout.log"; done
    ;;
  *)
    while IFS= read -r line; do printf '%s\\n' "$line" >> "$POSSE_TEST_ROOT/lead.log"; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), []byte(fakeAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "scout.md")
	if err := os.WriteFile(brief, []byte("---\ntype: scout\ntitle: Queued completion\ndone_when: report.md is complete\n---\nWrite a concise Report.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", "queued-completion"); !strings.Contains(output, "t1") {
		t.Fatalf("ride did not return t1: %s", output)
	}
	if !waitForCondition(15*time.Second, func() bool {
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		return err == nil && task.State == store.StateWorking
	}) {
		task, _ := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		t.Fatalf("Scout did not reach working: %#v", task)
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
		task, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		return err == nil && task.State == store.StateReported
	}) {
		task, _ := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
		log, _ := os.ReadFile(filepath.Join(fixture.root, "scout.log"))
		t.Fatalf("Scout did not report done: task=%#v log=%s", task, log)
	}

	message, err = fixture.db.MessageByID(context.Background(), message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if message.Status != "undeliverable" {
		t.Fatalf("queued message status after Scout completion = %q, want undeliverable", message.Status)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	foundNotice := false
	for _, notice := range notices {
		if notice.TaskID == message.TaskID && notice.Kind == "queued_message_undeliverable" && strings.Contains(notice.Summary, "#"+strconv.FormatInt(message.ID, 10)) && strings.Contains(notice.Summary, "posse send <task> <message>") {
			foundNotice = true
			break
		}
	}
	if !foundNotice {
		t.Fatalf("no actionable queued-message Notice for message #%d: %#v", message.ID, notices)
	}

	show := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	for _, expected := range []string{"undelivered_messages", strconv.FormatInt(message.ID, 10), "undeliverable", instruction} {
		if !strings.Contains(show, expected) {
			t.Fatalf("posse show omitted undelivered message field %q: %s", expected, show)
		}
	}
}
