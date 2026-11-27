//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestCodexSteerFailureAppearsInIdleNotice(t *testing.T) {
	t.Run("rollout present", func(t *testing.T) { testCodexSteerFailureNotice(t, false) })
	t.Run("rollout arrives late", func(t *testing.T) { testCodexSteerFailureNotice(t, true) })
}

func testCodexSteerFailureNotice(t *testing.T, late bool) {
	f := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = f.db.Close() })
	ctx := t.Context()
	client := herdr.NewWithEnv("herdr", f.env)
	const label, session, turn = "posse:shop:t1", "0193962c-0efe-7000-8000-000000000001", "0193962c-0efe-7000-8000-000000000002"
	codex := filepath.Join(f.root, "bin", "codex")
	script := strings.Replace(fakeBusyHarness, `        value = os.read(fd, 1)`, `        finish = os.path.join(root, "finish-codex-turn")
        if os.path.exists(finish):
            os.unlink(finish)
            os.write(1, b"\x1b]0;Codex\x07\r\n\r\n\xe2\x80\xba Ask Codex to do anything\r\n")
            subprocess.run(["herdr", "pane", "release-agent", pane, "--source", "posse.test", "--agent", "codex"], check=True, stdout=subprocess.DEVNULL)
        if not select.select([fd], [], [], 0.05)[0]:
            continue
        value = os.read(fd, 1)`, 1)
	if err := os.WriteFile(codex, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	worker, err := createTab(client, f.project.HerdrWorkspaceID, f.repo, label)
	if err != nil {
		t.Fatal(err)
	}
	paneID := worker.RootPane.PaneID
	if _, err := client.Run(ctx, "pane", "run", paneID, "POSSE_TEST_BUSY_NOW=1 exec "+shellQuote(codex)); err != nil {
		t.Fatal(err)
	}
	taskID, err := f.db.CreateTask(ctx, f.project.ID, store.Task{
		Seq: 1, Type: "change", Title: "Codex steer fixture", MergeMode: "pr", Profile: "deep", BaseRef: "main", Branch: "posse/t1",
		WorktreePath: f.repo, HerdrWorkspaceID: f.project.HerdrWorkspaceID, PaneID: paneID, PaneLabel: label,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.NextTaskLaunch(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "fixture started"); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		pane, ok := currentPane(t, client, paneID).(herdr.Pane)
		return ok && pane.AgentStatus == "working"
	}) {
		t.Fatalf("fake Codex never became ready: %#v", currentPane(t, client, paneID))
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "defaults.idle_after", "1ms")
	started := time.Now()
	sent := runPosse(t, f.binary, f.repo, f.leadEnv, "send", "t1", "Correct the current turn")
	if !strings.Contains(sent, "state: delivered") || !strings.Contains(sent, "reason: steered") {
		t.Fatalf("steer did not report transport receipt: %s", sent)
	}
	inputs := filepath.Join(f.root, "fake-agent-input.log")
	if !waitForCondition(3*time.Second, func() bool {
		data, _ := os.ReadFile(inputs)
		return strings.Contains(string(data), "Correct the current turn")
	}) {
		t.Fatal("Codex did not receive the steered instruction")
	}
	dir := filepath.Join(f.root, "codex", "sessions", "2026", "11", "27")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rollout := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}
{"timestamp":%q,"type":"event_msg","payload":{"type":"task_started","turn_id":%q}}
{"timestamp":%q,"type":"event_msg","payload":{"type":"task_complete","turn_id":%q,"error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"message\":\"unsupported websocket request type: response.interrupt\",\"type\":\"invalid_request_error\"}}","codex_error_info":"other"}}}
`, session, f.repo, started.UTC().Format(time.RFC3339Nano), turn, time.Now().UTC().Format(time.RFC3339Nano), turn)
	writeRollout := func() {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "rollout-fixture-"+session+".jsonl"), []byte(rollout), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if !late {
		writeRollout()
	}
	if _, err := client.Run(ctx, "pane", "report-agent-session", paneID, "--source", "herdr:codex", "--agent", "codex", "--agent-session-id", session, "--session-start-source", "startup"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "finish-codex-turn"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	noticesOfKind := func(kind string) []store.Notice {
		t.Helper()
		notices, err := f.db.Notices(ctx, f.project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		var matching []store.Notice
		for _, notice := range notices {
			if notice.TaskID == taskID && notice.Kind == kind {
				matching = append(matching, notice)
			}
		}
		return matching
	}
	if late {
		if !waitForCondition(15*time.Second, func() bool { return len(noticesOfKind("worker_idle")) == 1 }) {
			t.Fatal("generic idle Notice was not raised before the rollout arrived")
		}
		idle := noticesOfKind("worker_idle")[0]
		runPosse(t, f.binary, f.repo, f.leadEnv, "ack", fmt.Sprint(idle.ID))
		writeRollout()
	}
	if !waitForCondition(15*time.Second, func() bool { return len(noticesOfKind("codex_interrupt_rejected")) > 0 }) {
		t.Fatalf("recorded failure never surfaced; generic idle Notices: %+v", noticesOfKind("worker_idle"))
	}
	summary := noticesOfKind("codex_interrupt_rejected")[0].Summary
	if !strings.Contains(summary, "unsupported websocket request type: response.interrupt") || !strings.Contains(summary, "execution is unconfirmed") {
		t.Fatalf("failure Notice did not distinguish receipt from execution: %q", summary)
	}
	t.Log(summary)
	if !strings.Contains(sent, "execution is unconfirmed") {
		t.Fatalf("Codex steer output did not distinguish receipt from execution: %s", sent)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	if got := len(noticesOfKind("codex_interrupt_rejected")); got != 1 {
		t.Fatalf("failure Notices = %d, want one", got)
	}
	if got := len(noticesOfKind("worker_idle")); (!late && got != 0) || (late && got != 1) {
		t.Fatalf("unexpected number of generic idle Notices: %d (late rollout: %t)", got, late)
	}
	data, err := os.ReadFile(inputs)
	if err != nil || strings.Count(string(data), "Correct the current turn") != 1 || strings.Contains(string(data), "codex:continue") {
		t.Fatalf("Posse replayed the instruction or prompted a retry: %q (%v)", data, err)
	}
}
