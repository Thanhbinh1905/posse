//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestClaudeStopOverlapDeadlineAndGracefulStop(t *testing.T) {
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	// The test owns the poll claim below, so stop the independent Watch
	// before claiming it to avoid racing its startup poll.
	stopBackgroundWatch(t, f)
	ctx := context.Background()
	// Hold a genuine poll claim as another maintenance owner. Stop renewals
	// must neither overwrite it nor release another process's claim.
	pollToken, err := f.db.ClaimPRPoll(ctx, f.project.ID, time.Now(), time.Minute, true)
	if err != nil || pollToken == "" {
		t.Fatalf("claim PR poll: %q %v", pollToken, err)
	}
	defer f.db.ReleasePRPoll(ctx, f.project.ID, pollToken)
	transcript := filepath.Join(f.root, "hook-transcript.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"hook_event_name": "Stop", "session_id": "fixture-session", "transcript_path": transcript})
	newHook := func(timeout string) *exec.Cmd {
		command := exec.Command(f.binary, "_claude-stop", "--timeout", timeout)
		command.Dir, command.Env, command.Stdin = f.repo, f.leadEnv, bytes.NewReader(input)
		return command
	}
	waitForHook := func() {
		t.Helper()
		if !waitForCondition(3*time.Second, func() bool {
			lock, err := os.OpenFile(filepath.Join(f.home, "projects", "shop", "claude-stop.lock"), os.O_RDWR, 0)
			if err != nil {
				return false
			}
			defer lock.Close()
			err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			}
			return err == syscall.EWOULDBLOCK
		}) {
			t.Fatal("Stop hook did not own the Project wait")
		}
	}
	first := newHook("600")
	var firstOutput bytes.Buffer
	first.Stdout, first.Stderr = &firstOutput, &firstOutput
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer first.Process.Kill()
	waitForHook()
	if output, err := newHook("600").CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("overlapping Stop claimed a second wait: %v %s", err, output)
	}
	if err := first.Wait(); err == nil || !strings.Contains(firstOutput.String(), "Posse Notice wait renewed") {
		t.Fatalf("Stop did not renew near its bound: %v %s", err, firstOutput.String())
	}
	stopping := newHook("5000")
	var stoppedOutput bytes.Buffer
	stopping.Stdout, stopping.Stderr = &stoppedOutput, &stoppedOutput
	if err := stopping.Start(); err != nil {
		t.Fatal(err)
	}
	defer stopping.Process.Kill()
	waitForHook()
	if err := stopping.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := stopping.Wait(); err != nil || stoppedOutput.Len() != 0 {
		t.Fatalf("graceful Stop re-invoked Claude: %v %s", err, stoppedOutput.String())
	}
	id, err := f.db.CreateNotice(ctx, store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "pending after graceful stop", DataJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	output, err := newHook("600").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "pending after graceful stop") || !strings.Contains(string(output), "Posse batch receipt:") {
		t.Fatalf("restart lost pending Notice %d: %v %s", id, err, output)
	}
	// A missing persisted receipt is held, not replayed. Once the warning
	// itself is recorded, another Stop must not create a hot wake loop.
	warning, err := newHook("600").CombinedOutput()
	if err == nil || !strings.Contains(string(warning), "Posse uncertain receipt:") {
		t.Fatalf("missing receipt was not held: %v %s", err, warning)
	}
	entry, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": string(warning)}})
	if err := os.WriteFile(transcript, append(entry, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := newHook("600").CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("unresolved receipt caused another wake: %v %s", err, output)
	}
	var receipt string
	if err := f.db.QueryRowContext(ctx, `SELECT delivery_id FROM notice_delivery_receipts WHERE project_id=?`, f.project.ID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "watch", "--receipt", receipt, "--receipt-outcome", "accepted")
	if output, err := newHook("600").CombinedOutput(); err == nil || !strings.Contains(string(output), "Posse Notice wait renewed") {
		t.Fatalf("explicit resolution did not rearm the next Stop: %v %s", err, output)
	}
	var after string
	if err := f.db.QueryRowContext(ctx, `SELECT pr_poll_claim_token FROM project_watch_state WHERE project_id=?`, f.project.ID).Scan(&after); err != nil || after != pollToken {
		t.Fatalf("Stop restart stole or released an active PR poll: %q, want %q: %v", after, pollToken, err)
	}
	var receipts int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notice_delivery_receipts WHERE project_id=?`, f.project.ID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("pending Notice was claimed more than once: %d %v", receipts, err)
	}
}

// A local Anthropic stream drives the real harness without credentials or an
// external model. The isolated Herdr/Posse fixture remains the production CLI.
func claudeLocalModel(t *testing.T, respond func(int, string) []map[string]any) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		blocks := respond(int(calls.Add(1)), string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(name string, data any) {
			encoded, _ := json.Marshal(data)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
		}
		event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_local", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}})
		reason := "end_turn"
		for index, block := range blocks {
			start := map[string]any{"type": block["type"]}
			delta := map[string]any{"type": "text_delta", "text": block["text"]}
			if block["type"] == "tool_use" {
				reason = "tool_use"
				start["id"], start["name"], start["input"] = block["id"], block["name"], map[string]any{}
				encoded, _ := json.Marshal(block["input"])
				delta = map[string]any{"type": "input_json_delta", "partial_json": string(encoded)}
			} else {
				start["text"] = ""
			}
			event("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": start})
			event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta})
			event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		}
		event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
		event("message_stop", map[string]any{"type": "message_stop"})
	}))
	t.Cleanup(server.Close)
	return server
}

func realClaudeEnv(f *prLifecycleFixture, server string) []string {
	env := make([]string, 0, len(f.leadEnv))
	for _, entry := range f.leadEnv {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_") || key == "CLAUDECODE" || key == "XDG_RUNTIME_DIR" {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range map[string]string{
		"CLAUDE_CONFIG_DIR": filepath.Join(f.root, "real-claude"), "ANTHROPIC_BASE_URL": server,
		"ANTHROPIC_API_KEY": "local-test-only", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "XDG_RUNTIME_DIR": filepath.Join(f.root, "runtime"),
	} {
		env = setEnv(env, key, value)
	}
	return env
}

func TestRealClaudeWatchBackgroundLimit(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("real Claude executable is not installed")
	}
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	backgrounded := make(chan struct{}, 1)
	server := claudeLocalModel(t, func(call int, body string) []map[string]any {
		if call == 1 {
			return []map[string]any{{"type": "tool_use", "id": "limit_probe", "name": "Bash", "input": map[string]any{"command": "exec " + shellQuote(f.binary) + " watch --timeout 5000", "run_in_background": true, "timeout": 1000, "description": "Observe Watch at the background ceiling"}}}
		}
		if call == 2 {
			backgrounded <- struct{}{}
		}
		return []map[string]any{{"type": "text", "text": "Probe complete."}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := execgroup.CommandContext(ctx, claude, "-p", "--dangerously-skip-permissions", "--setting-sources", "", "--model", "claude-sonnet-4-6", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	command.Dir, command.Env = f.repo, realClaudeEnv(f, server.URL)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = command.Cancel(); _ = command.Wait() }()
	lines := make(chan string, 128)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	_, _ = io.WriteString(stdin, "{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"Run the background probe.\"}}\n")
	select {
	case <-backgrounded:
	case <-ctx.Done():
		t.Fatal("Claude did not start its background Watch")
	}
	for {
		select {
		case line, open := <-lines:
			if !open {
				t.Fatal("Claude exited before reporting the background deadline")
			}
			if strings.Contains(line, "was stopped after reaching its background time limit") {
				t.Log("real Claude: 5s Watch stopped by the configured 1000ms background deadline; no harness clock or code replacement")
				t.Log(line)
				return
			}
		case <-ctx.Done():
			t.Fatal("real Claude did not enforce its configured background deadline")
		}
	}
}

func TestRealClaudeStopRenewsAndDeliversPendingNotice(t *testing.T) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("real Claude executable is not installed")
	}
	f := newPRLifecycleFixtureWithLead(t, true)
	defer f.db.Close()
	plugin := filepath.Join(f.home, "projects", "shop", "lead-claude-watch")
	hookFile := filepath.Join(plugin, "hooks", "hooks.json")
	// Shorten only Posse's wait, leaving the real harness lifecycle unchanged.
	hooks := readTestFile(t, hookFile)
	if err := os.WriteFile(hookFile, []byte(strings.Replace(hooks, " _claude-stop", " _claude-stop --timeout 700", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 16)
	server := claudeLocalModel(t, func(call int, body string) []map[string]any {
		requests <- body
		return []map[string]any{{"type": "text", "text": fmt.Sprintf("Turn %d complete.", call)}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	command := execgroup.CommandContext(ctx, claude, "-p", "--dangerously-skip-permissions", "--setting-sources", "", "--model", "claude-sonnet-4-6", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--plugin-dir", plugin)
	command.Dir, command.Env = f.repo, realClaudeEnv(f, server.URL)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(f.root, "claude-stop.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		_ = stdin.Close()
		if !finished {
			_ = command.Cancel()
			_ = command.Wait()
		}
	}()
	_, _ = io.WriteString(stdin, "{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"Start the Notice wait.\"}}\n")
	next := func() string {
		t.Helper()
		select {
		case body := <-requests:
			return body
		case <-ctx.Done():
			t.Fatalf("Claude Stop did not wake: %s", readTestFile(t, log.Name()))
			return ""
		}
	}
	next()
	// Async wakes must keep working beyond the synchronous Stop hook's
	// consecutive-continuation cap, without a User turn or a detached relay.
	for renewal := 0; renewal < 10; renewal++ {
		if body := next(); !strings.Contains(body, "Posse Notice wait renewed") {
			t.Fatalf("bounded Stop renewal %d did not reach Claude: %s", renewal+1, body)
		}
	}
	notice, err := f.db.CreateNotice(context.Background(), store.Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "pending across Stop renewal", DataJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if delivery := next(); !strings.Contains(delivery, "pending across Stop renewal") || !strings.Contains(delivery, "Posse batch receipt:") {
		t.Fatalf("restarted Stop lost the pending Notice: %s", delivery)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "ack", fmt.Sprint(notice))
	if !waitForCondition(8*time.Second, func() bool {
		var state string
		err := f.db.QueryRowContext(context.Background(), `SELECT state FROM notice_delivery_receipts WHERE project_id=?`, f.project.ID).Scan(&state)
		return err == nil && state == "accepted"
	}) {
		t.Fatalf("Claude transcript receipt not accepted: %s", readTestFile(t, log.Name()))
	}
	var delivered, acked int64
	if err := f.db.QueryRowContext(context.Background(), `SELECT delivered_at,acked_at FROM notices WHERE id=?`, notice).Scan(&delivered, &acked); err != nil || delivered == 0 || acked == 0 {
		t.Fatalf("receipt lost the acknowledgment: delivered=%d acked=%d err=%v", delivered, acked, err)
	}
	// Closing the harness input owns teardown of its async hook. No relay is
	// detached, and a normal exit must not require killing the process group.
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatalf("Claude graceful shutdown: %v\n%s", err, readTestFile(t, log.Name()))
		}
	case <-time.After(8 * time.Second):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		finished = true
		t.Fatal("Claude did not stop its owned wait")
	}
	var receipts int
	if err := f.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notice_delivery_receipts WHERE project_id=?`, f.project.ID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("Notice double-claimed across Stop restarts: receipts=%d err=%v", receipts, err)
	}
	t.Log("real Claude asyncRewake: quiet deadline renewed, pending Notice delivered, transcript receipt accepted, acknowledgment preserved, graceful session stop")
}
