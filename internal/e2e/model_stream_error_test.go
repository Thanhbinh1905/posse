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
	posseRuntime "github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestPiStreamDisconnectAutomaticallyResumesWithinBound(t *testing.T) {
	fixture := newPiModelErrorFixture(t, "Error: stream error: stream disconnected before completion: stream closed before response.completed", true)
	started := time.Now()
	fixture.emitError(t)

	if !waitForCondition(15*time.Second, func() bool {
		return countPromptLines(fixture.prompts, "continue") == 3 && fixture.hasNotice("model_stream_error")
	}) {
		logged, _ := os.ReadFile(fixture.prompts)
		ingest, _ := os.ReadFile(filepath.Join(fixture.base.root, "ingest.log"))
		task, taskErr := fixture.base.db.Task(context.Background(), fixture.base.project.ID, "t1")
		episode, _ := fixture.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
		signals, _ := fixture.base.db.TaskSignals(context.Background(), task.ID, 30)
		screen, _ := (posseRuntime.SystemProgress{}).ReadPane(context.Background(), fixture.client, task.PaneID, 200)
		t.Fatalf("stream-error recovery did not exhaust its bounded episode within seconds: task=%#v task_err=%v prompts=%q episode=%#v signals=%#v screen=%q ingest=%q", task, taskErr, logged, episode, signals, screen, ingest)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("stream-error recovery took %s, want within seconds", elapsed)
	}
	if got := countPromptLines(fixture.prompts, "continue"); got != 3 {
		t.Fatalf("automatic continue nudges = %d, want the per-episode bound of 3", got)
	}
	show := runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv, "show", "--full", "t1")
	for _, expected := range []string{"model_stream_error", "continue", "attempts", "exhausted"} {
		if !strings.Contains(strings.ToLower(show), strings.ToLower(expected)) {
			t.Fatalf("posse show omitted stream-error episode detail %q: %s", expected, show)
		}
	}
}

func TestPiProviderRefusalIsNotRetried(t *testing.T) {
	fixture := newPiModelErrorFixture(t, "This content was flagged for possible cybersecurity risk", false)
	fixture.emitError(t)
	if !waitForCondition(4*time.Second, func() bool { return fixture.hasNotice("model_refused") }) {
		t.Fatal("provider refusal did not produce a specific Notice within seconds")
	}
	if got := countPromptLines(fixture.prompts, "continue"); got != 0 {
		logged, _ := os.ReadFile(fixture.prompts)
		t.Fatalf("provider refusal was automatically retried: continue nudges=%d prompts=%q", got, logged)
	}
	show := runPosse(t, fixture.base.binary, fixture.base.repo, fixture.base.leadEnv, "show", "t1")
	if !strings.Contains(show, "model_refused") || !strings.Contains(show, "refused") {
		t.Fatalf("provider refusal episode not visible in posse show: %s", show)
	}
}

func TestPiStreamDisconnectIsNotNudgedIntoFocusedRider(t *testing.T) {
	fixture := newPiModelErrorFixture(t, "Error: stream error: stream disconnected before completion: stream closed before response.completed", true)
	task := fixture.base.mustTask(t, "t1")
	if _, err := fixture.client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": task.PaneID}); err != nil {
		t.Fatalf("focus the isolated Rider pane: %v", err)
	}
	fixture.emitError(t)
	if !waitForCondition(4*time.Second, func() bool { return fixture.hasNotice("model_stream_error") }) {
		t.Fatal("focused stream-error episode did not produce a specific Notice")
	}
	if got := countPromptLines(fixture.prompts, "continue"); got != 0 {
		logged, _ := os.ReadFile(fixture.prompts)
		t.Fatalf("focused Rider received an automatic nudge: count=%d prompts=%q", got, logged)
	}
	signals, err := fixture.base.db.TaskSignals(context.Background(), task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundWithheld := false
	for _, signal := range signals {
		if signal.Verb == "model_stream_error_withheld" && strings.Contains(signal.Note, "focused") {
			foundWithheld = true
		}
	}
	if !foundWithheld {
		t.Fatalf("focused episode did not record why the nudge was withheld: %#v", signals)
	}
}

const e2ePiErrorHelpLine = "If this looks like a pi bug, /bug sends a report to the developers."

type piModelErrorFixture struct {
	base    *prLifecycleFixture
	client  *herdr.Client
	prompts string
	ready   string
	emit    string
}

func newPiModelErrorFixture(t *testing.T, modelError string, repeatOnContinue bool) *piModelErrorFixture {
	t.Helper()
	base := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = base.db.Close() })
	if err := os.MkdirAll(filepath.Join(base.root, "pi", "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", base.env)
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration in isolated Herdr: %v", err)
	}
	if output := runPosse(t, base.binary, base.repo, base.leadEnv, "config", "set", "profiles.deep.kind", "pi"); !strings.Contains(output, "pi") {
		t.Fatalf("select Pi for the Rider harness: %s", output)
	}
	if !strings.Contains(modelError, "\n") && !strings.HasPrefix(strings.TrimSpace(modelError), "Error:") {
		modelError = "Error: " + modelError
	}
	prompts := filepath.Join(base.root, "pi-prompts.log")
	ready := filepath.Join(base.root, "pi-ready")
	emit := filepath.Join(base.root, "emit-model-error")
	retryOnContinue := "false"
	if repeatOnContinue {
		retryOnContinue = "true"
	}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "${1:-}" = "--version" ]; then echo 'pi 1.0.0'; exit 0; fi
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent pi --state idle >/dev/null 2>&1
IFS= read -r prompt || exit 0
printf '%%s\n' "$prompt" >> "$POSSE_TEST_ROOT/pi-prompts.log"
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent pi --state working >/dev/null 2>&1
: > "$POSSE_TEST_ROOT/pi-ready"
while [ ! -e "$POSSE_TEST_ROOT/emit-model-error" ]; do sleep 0.02; done
report_status() {
  state=$1
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent pi --state "$state" >/dev/null 2>&1
  if [ "$state" = idle ] && [ "${attempt:-0}" -gt 0 ] && [ -e "$POSSE_TEST_ROOT/pause-retry-idle-hook" ]; then
    : > "$POSSE_TEST_ROOT/retry-idle-ready"
    while [ ! -e "$POSSE_TEST_ROOT/release-retry-idle-hook" ]; do sleep 0.02; done
  fi
  HERDR_PLUGIN_EVENT_JSON="{\"event\":\"pane.agent_status_changed\",\"data\":{\"pane_id\":\"$HERDR_PANE_ID\",\"agent_status\":\"$state\"}}" "$POSSE_E2E_POSSE_BIN" _ingest >> "$POSSE_TEST_ROOT/ingest.log" 2>&1 &
  ingest_pid=$!
  printf '%%s\n' "$ingest_pid" > "$POSSE_TEST_ROOT/ingest-pid"
  wait "$ingest_pid" 2>/dev/null || true
}
report_idle() { report_status idle; }
report_working() { report_status working; }
if [ -e "$POSSE_TEST_ROOT/clear-model-screen" ]; then printf '\033[2J\033[H'; fi
printf '%%s\n%%s\n' %s %s
report_idle
attempt=0
while IFS= read -r instruction; do
  printf '%%s\n' "$instruction" >> "$POSSE_TEST_ROOT/pi-prompts.log"
  if [ "$instruction" = continue ] && [ %s = true ]; then
    attempt=$((attempt + 1))
    report_working
    if [ -e "$POSSE_TEST_ROOT/clear-model-screen" ]; then printf '\033[2J\033[H'; else printf 'Retry turn %%s\n' "$attempt"; fi
    printf '%%s\n%%s\n' %s %s
    report_idle
  fi
done
`, shellQuote(modelError), shellQuote(e2ePiErrorHelpLine), retryOnContinue, shellQuote(modelError), shellQuote(e2ePiErrorHelpLine))
	if err := os.WriteFile(filepath.Join(base.root, "bin", "pi"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(base.root, "model-error.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Recover model error\ndone_when: model error is handled\n---\nContinue the task.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, base.binary, base.repo, base.leadEnv, "ride", "--brief", brief, "--name", "recover-model-error"); !strings.Contains(output, "t1") {
		t.Fatalf("ride did not return t1: %s", output)
	}
	if !waitForCondition(5*time.Second, func() bool {
		task, err := base.db.Task(context.Background(), base.project.ID, "t1")
		_, readyErr := os.Stat(ready)
		return err == nil && task.State == store.StateWorking && readyErr == nil
	}) {
		t.Fatal("Pi fake agent did not reach its working turn")
	}
	return &piModelErrorFixture{base: base, client: client, prompts: prompts, ready: ready, emit: emit}
}

func (fixture *piModelErrorFixture) emitError(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(fixture.emit, []byte("emit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *piModelErrorFixture) hasNotice(kind string) bool {
	notices, err := fixture.base.db.Notices(context.Background(), fixture.base.project.ID, false)
	if err != nil {
		return false
	}
	for _, notice := range notices {
		if notice.Kind == kind {
			return true
		}
	}
	return false
}

func countPromptLines(path, want string) int {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(contents), "\n") {
		if line == want {
			count++
		}
	}
	return count
}
