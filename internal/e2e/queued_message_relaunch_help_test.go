//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRelaunchedShowExplainsMixedMessageStatuses(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	fakeAgent := `#!/bin/sh
set -eu
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    while [ ! -e "$POSSE_E2E_SIGNAL_GATE/t1" ]; do sleep 0.02; done
    if [ "$(cat "$POSSE_E2E_SIGNAL_GATE/t1")" = failed ]; then
      "$POSSE_E2E_POSSE_BIN" holler failed 'Controlled failure without idling' >> "$POSSE_TEST_ROOT/scout.log" 2>&1
    fi
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
    while IFS= read -r instruction; do printf '%s\n' "$instruction" >> "$POSSE_TEST_ROOT/scout.log"; done
    ;;
  *)
    while IFS= read -r line; do :; done
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), []byte(fakeAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "defaults.auto_recover", "false"); !strings.Contains(output, "auto_recover") {
		t.Fatalf("could not disable automatic recovery: %s", output)
	}
	brief := filepath.Join(fixture.root, "scout.md")
	if err := os.WriteFile(brief, []byte("---\ntype: scout\ntitle: Relaunched message help\ndone_when: report.md is complete\n---\nWrite a concise Report.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", "relaunched-message-help"); !strings.Contains(output, "t1") {
		t.Fatalf("ride did not return t1: %s", output)
	}
	fixture.waitTaskState(t, "t1", store.StateWorking)
	task := fixture.mustTask(t, "t1")
	waitForRelaunchHelpAgentStatus(t, fixture, task.PaneID, "working")

	const undeliverableBody = "T180 instruction stranded by failure"
	oldMessageID := queueRelaunchHelpMessage(t, fixture, task.ID, undeliverableBody)
	gate := filepath.Join(fixture.root, "signal-gates", "t1")
	if err := os.WriteFile(gate, []byte("failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.waitTaskState(t, "t1", store.StateFailed)
	oldMessage, err := fixture.db.MessageByID(context.Background(), oldMessageID)
	if err != nil || oldMessage.Status != "undeliverable" {
		t.Fatalf("pre-relaunch message = %#v, %v; want undeliverable", oldMessage, err)
	}

	if err := os.WriteFile(gate, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "relaunch", "t1"); !strings.Contains(output, "working") {
		t.Fatalf("relaunch did not resume the Scout: %s", output)
	}
	fixture.waitTaskState(t, "t1", store.StateWorking)
	task = fixture.mustTask(t, "t1")
	waitForRelaunchHelpAgentStatus(t, fixture, task.PaneID, "idle")

	client := herdr.NewWithEnv("herdr", fixture.env)
	if _, err := client.Run(context.Background(), "pane", "report-agent", task.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "working"); err != nil {
		t.Fatalf("mark replacement Scout working: %v", err)
	}
	waitForRelaunchHelpAgentStatus(t, fixture, task.PaneID, "working")

	const queuedBody = "T180 instruction queued for the replacement Scout"
	queuedMessageID := queueRelaunchHelpMessage(t, fixture, task.ID, queuedBody)
	show := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1")
	for _, expected := range []string{
		fmt.Sprintf("%d,%s,undeliverable", oldMessageID, undeliverableBody),
		fmt.Sprintf("%d,%s,queued", queuedMessageID, queuedBody),
		"Undeliverable instructions are not retried automatically; resend each with `posse send t1 <message>`",
		"Queued instructions will be delivered when the Rider is ready and unfocused",
	} {
		if !strings.Contains(show, expected) {
			t.Fatalf("posse show omitted %q: %s", expected, show)
		}
	}
	scoutLog, err := os.ReadFile(filepath.Join(fixture.root, "scout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(scoutLog), undeliverableBody) {
		t.Fatalf("replacement Scout received the old undeliverable instruction: %s", scoutLog)
	}
	for _, message := range []struct {
		id     int64
		status string
	}{{oldMessageID, "undeliverable"}, {queuedMessageID, "queued"}} {
		stored, err := fixture.db.MessageByID(context.Background(), message.id)
		if err != nil || stored.Status != message.status {
			t.Errorf("message #%d status = %#v, %v; want %q", message.id, stored, err, message.status)
		}
	}
}

func queueRelaunchHelpMessage(t *testing.T, fixture *prLifecycleFixture, taskID int64, body string) int64 {
	t.Helper()
	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", body, "--queue")
	if !strings.Contains(output, "state: queued") {
		t.Fatalf("message was not queued: %s", output)
	}
	var messageID int64
	if err := fixture.db.QueryRowContext(context.Background(), "SELECT id FROM messages WHERE task_id=? AND body=?", taskID, body).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	return messageID
}

func waitForRelaunchHelpAgentStatus(t *testing.T, fixture *prLifecycleFixture, paneID, status string) {
	t.Helper()
	client := herdr.NewWithEnv("herdr", fixture.env)
	if !waitForCondition(10*time.Second, func() bool {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID == paneID && (pane.AgentStatus == status || status == "idle" && pane.AgentStatus == "done") {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("pane %s did not become %s", paneID, status)
	}
}
