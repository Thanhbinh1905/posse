//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	posseRuntime "github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

const t242StreamError = "Error: stream error: stream disconnected before completion: stream closed before response.completed"

func TestT242OrdinaryToolOutputIsNotRetried(t *testing.T) {
	f := newPiModelErrorFixture(t, "$ grep stream-error rider.log\n2026-10-06 log entry: "+t242StreamError, false)
	f.emitError(t)
	time.Sleep(1200 * time.Millisecond)
	prompts, _ := os.ReadFile(f.prompts)
	if n := countPromptLines(f.prompts, "continue"); n != 0 {
		t.Fatalf("ordinary successful grep output received %d automatic retries: prompts=%q", n, prompts)
	}
}

func TestT242QuotedRefusalTranscriptIsNotRetried(t *testing.T) {
	quoted := "The User asked about: Error: This content was flagged for possible cybersecurity risk\n" + e2ePiErrorHelpLine
	f := newPiModelErrorFixture(t, quoted, false)
	f.emitError(t)
	time.Sleep(1200 * time.Millisecond)
	if n := countPromptLines(f.prompts, "continue"); n != 0 {
		t.Fatalf("quoted provider refusal received %d automatic retries", n)
	}
}

func TestT242DecisionWaitIsNotRetried(t *testing.T) {
	f := newPiModelErrorFixture(t, t242StreamError, false)
	task := f.base.mustTask(t, "t1")
	riderEnv := setEnv(f.base.leadEnv, "HERDR_PANE_ID", task.PaneID)
	riderEnv = setEnv(riderEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	runPosse(t, f.base.binary, task.WorktreePath, riderEnv, "holler", "needs-decision", "Waiting on Lead")
	f.emitError(t)
	time.Sleep(1200 * time.Millisecond)
	if n := countPromptLines(f.prompts, "continue"); n != 0 {
		t.Fatalf("waiting Rider received %d nudges", n)
	}
}

func TestT242DecisionDuringBackoffIsNotRetried(t *testing.T) {
	f := newPiModelErrorFixture(t, t242StreamError, false)
	task := f.base.mustTask(t, "t1")
	f.emitError(t)
	t242WaitClaim(t, f, task)
	riderEnv := setEnv(f.base.leadEnv, "HERDR_PANE_ID", task.PaneID)
	riderEnv = setEnv(riderEnv, "HERDR_WORKSPACE_ID", task.HerdrWorkspaceID)
	runPosse(t, f.base.binary, task.WorktreePath, riderEnv, "holler", "needs-decision", "Waiting on Lead")
	signals, err := f.base.db.TaskSignals(context.Background(), task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range signals {
		if signal.Verb == "needs-decision" {
			t.Logf("needs-decision committed at %d", signal.At)
		}
	}
	time.Sleep(900 * time.Millisecond)
	state := f.base.mustTask(t, "t1").State
	if n := countPromptLines(f.prompts, "continue"); n != 0 {
		t.Fatalf("Rider entered %q during backoff but still received %d nudges", state, n)
	}
}

func TestT242IdenticalRepaintDoesNotStrandRecovery(t *testing.T) {
	f := newPiModelErrorFixture(t, t242StreamError, true)
	task := f.base.mustTask(t, "t1")
	if err := os.WriteFile(filepath.Join(f.base.root, "clear-model-screen"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	f.emitError(t)
	if !waitForCondition(5*time.Second, func() bool { return f.hasNotice("model_stream_error") }) {
		episode, _ := f.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
		current, _ := f.base.db.Task(context.Background(), f.base.project.ID, "t1")
		signals, _ := f.base.db.TaskSignals(context.Background(), task.ID, 20)
		var events []string
		rows, queryErr := f.base.db.QueryContext(context.Background(), `SELECT id,kind,data_json FROM events ORDER BY id`)
		if queryErr == nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var kind, data string
				if rows.Scan(&id, &kind, &data) == nil {
					events = append(events, fmt.Sprintf("%d:%s:%s", id, kind, data))
				}
			}
		}
		ingestLog, _ := os.ReadFile(filepath.Join(f.base.root, "ingest.log"))
		t.Fatalf("repainted identical stream failures stranded recovery: prompts=%d idle_since=%d episode=%#v signals=%#v events=%q ingest=%q", countPromptLines(f.prompts, "continue"), current.IdleSince, episode, signals, events, ingestLog)
	}
	episode, err := f.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
	if err != nil || episode.Status != "exhausted" || episode.Attempts != 3 || episode.NextAttemptAt != 0 {
		t.Fatalf("failed stream turn did not deterministically exhaust the retry budget: episode=%#v err=%v", episode, err)
	}
	if prompts := countPromptLines(f.prompts, "continue"); prompts != 3 {
		t.Fatalf("retry count=%d, want exactly 3", prompts)
	}
	notices, err := f.base.db.Notices(context.Background(), f.base.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == "model_stream_error" && strings.Contains(notice.Summary, "stopped after 3 attempts") {
			return
		}
	}
	t.Fatalf("exhausted episode lacks its specific Notice: %#v", notices)
}

func TestT242InterruptedIngestRecoversOrRaisesNotice(t *testing.T) {
	f := newPiModelErrorFixture(t, t242StreamError, false)
	task := f.base.mustTask(t, "t1")
	f.emitError(t)
	t242WaitClaim(t, f, task)
	pidBytes, err := os.ReadFile(filepath.Join(f.base.root, "ingest-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	// A fresh CLI process replays the event and runs normal reconciliation.
	event := fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"idle"}}`, task.PaneID)
	env := setEnv(f.base.env, "HERDR_PLUGIN_EVENT_JSON", event)
	runPosse(t, f.base.binary, f.base.repo, env, "_ingest")
	runPosse(t, f.base.binary, f.base.repo, f.base.leadEnv, "recover", "--all")
	runPosse(t, f.base.binary, f.base.repo, f.base.leadEnv, "show", "t1")
	// Check the fallback without waiting three real minutes.
	snapshot, err := f.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := posseRuntime.ReconcileSnapshot(context.Background(), f.base.db, f.base.project.ID, snapshot, time.Now().Add(time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	episode, err := f.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	prompts := countPromptLines(f.prompts, "continue")
	modelNotice, idleNotice := f.hasNotice("model_stream_error"), f.hasNotice("worker_idle")
	current := f.base.mustTask(t, "t1")
	t.Logf("after interruption/recovery: prompts=%d model_notice=%t idle_notice=%t episode=%#v state=%s launches=%d", prompts, modelNotice, idleNotice, episode, current.State, current.Launches)
	if prompts == 0 && !modelNotice && !idleNotice {
		t.Fatalf("interrupted claim remains invisible after fresh ingest/recover/show and idle deadline: prompts=%d model_notice=%t idle_notice=%t episode=%#v", prompts, modelNotice, idleNotice, episode)
	}
}

func TestT242QueuedLeadInstructionWins(t *testing.T) {
	f := newPiModelErrorFixture(t, t242StreamError, false)
	runPosse(t, f.base.binary, f.base.repo, f.base.leadEnv, "send", "t1", "Wait for the Lead's next instruction", "--queue")
	f.emitError(t)
	if !waitForCondition(3*time.Second, func() bool { return f.hasNotice("model_stream_error") }) {
		t.Fatal("queued instruction did not get specific notice")
	}
	if n := countPromptLines(f.prompts, "continue"); n != 0 {
		t.Fatalf("queued Lead instruction received %d nudges", n)
	}
}

func TestT242BackoffIsPersistedAndBounded(t *testing.T) {
	f := newPiModelErrorFixture(t, t242StreamError, true)
	f.emitError(t)
	if !waitForCondition(8*time.Second, func() bool { return f.hasNotice("model_stream_error") }) {
		t.Fatal("episode did not exhaust")
	}
	task := f.base.mustTask(t, "t1")
	signals, err := f.base.db.TaskSignals(context.Background(), task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	delays := map[int]int64{}
	for _, signal := range signals {
		if signal.Verb != "model_stream_error_retry" {
			continue
		}
		var data struct {
			Attempt int   `json:"attempt"`
			Backoff int64 `json:"backoff_ms"`
		}
		if err := json.Unmarshal([]byte(signal.DataJSON), &data); err != nil {
			t.Fatal(err)
		}
		delays[data.Attempt] = data.Backoff
	}
	if len(delays) != 3 || delays[1] != 250 || delays[2] != 500 || delays[3] != 1000 {
		episode, _ := f.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
		prompts, _ := os.ReadFile(f.prompts)
		t.Fatalf("retry history backoff=%v episode=%#v prompts=%q signals=%#v", delays, episode, prompts, signals)
	}
	time.Sleep(1200 * time.Millisecond)
	if n := countPromptLines(f.prompts, "continue"); n != 3 {
		t.Fatalf("retry bound=%d, want 3", n)
	}
}

func t242WaitClaim(t *testing.T, f *piModelErrorFixture, task store.Task) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		episode, err := f.base.db.TaskModelErrorEpisode(context.Background(), task.ID)
		if err == nil && episode.Attempts == 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("nudge claim was not observed")
}
