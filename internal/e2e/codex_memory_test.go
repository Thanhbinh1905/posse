//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestCodexMemoryCannotReopenCompletedAuthorDuringReview(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("installed Codex CLI required")
	}
	codex, err = filepath.EvalSymlinks(codex)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "API_KEY") || strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CODEX_") {
			t.Setenv(key, "")
		}
	}
	version, err := exec.Command(codex, "--version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Installed harness: %s", version)
	f := reviewIdentityFixture(t)
	release := make(chan struct{})
	started := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var mu sync.Mutex
	memoryRequests, rootRequests := 0, 0
	followupIssued := false
	memoryFinished := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data, _ := json.Marshal(body)
		toolOutput := ""
		if inputs, ok := body["input"].([]any); ok {
			for _, input := range inputs {
				item, _ := input.(map[string]any)
				if item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output" {
					toolOutput += fmt.Sprint(item["output"])
					t.Logf("Tool output: %v", item)
				}
			}
		}
		meta, _ := body["client_metadata"].(map[string]any)
		metadata := fmt.Sprint(meta["x-codex-turn-metadata"])
		memory := strings.Contains(metadata, `"turn_trigger":"memory_consolidation"`)
		root := strings.Contains(metadata, `"thread_source":"user"`)
		command, id := "", "root"
		firstMemory := false
		mu.Lock()
		if memory {
			memoryRequests++
			id = "memory"
			firstMemory = memoryRequests == 1
			if firstMemory {
				command = `printf 'pid=%s pane=%s home=%s cwd=%s\n' "$$" "$HERDR_PANE_ID" "$POSSE_HOME" "$PWD" > "$POSSE_TEST_ROOT/memory-identity"; "$POSSE_E2E_POSSE_BIN" signal working "Harness memory consolidation" > "$POSSE_TEST_ROOT/memory-signal" 2>&1`
			} else {
				select {
				case memoryFinished <- toolOutput:
				default:
				}
			}
		} else if root {
			rootRequests++
			if strings.Contains(string(data), "AUTHORIZED_FOLLOWUP") && !followupIssued {
				followupIssued = true
				command = `result=$("$POSSE_E2E_POSSE_BIN" signal working "Authorized Lead follow-up"); printf '%s\n' "$result" > "$POSSE_TEST_ROOT/followup-signal"; token=$(printf '%s\n' "$result" | awk -F '"' '/turn_token:/ {print $2}'); "$POSSE_E2E_POSSE_BIN" signal done "Authorized follow-up complete" --turn "$token" >> "$POSSE_TEST_ROOT/followup-signal" 2>&1`
				id = "followup"
			} else if rootRequests == 1 {
				command = `printf 'author change\n' > memory-author.txt; git add memory-author.txt; git commit -m 'Author change'; "$POSSE_E2E_POSSE_BIN" signal done "Author complete" --turn launch:1`
			}
		}
		mu.Unlock()
		t.Logf("Codex request memory=%t provenance=%s", memory, metadata)
		if root {
			select {
			case <-started:
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Second):
				http.Error(w, "start barrier timed out", 408)
				return
			}
		}
		if memory {
			if firstMemory {
				t.Logf("Real Phase 2 request SHA-256: %x", sha256.Sum256(data))
			}
			if !strings.Contains(string(data), "Memory Writing Agent: Phase 2") {
				http.Error(w, "missing real prompt", 400)
				return
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			case <-time.After(45 * time.Second):
				http.Error(w, "memory barrier timed out", 408)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(typ string, value map[string]any) {
			value["type"] = typ
			encoded, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, encoded)
		}
		emit("response.created", map[string]any{"response": map[string]any{"id": "response-" + id}})
		item := map[string]any{"type": "message", "id": "message-" + id, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Complete."}}}
		if command != "" {
			args, _ := json.Marshal(map[string]any{"cmd": command, "login": false, "yield_time_ms": 1000, "max_output_tokens": 3000})
			item = map[string]any{"type": "function_call", "id": "item-" + id, "call_id": "call-" + id, "name": "exec_command", "arguments": string(args)}
		}
		emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
		emit("response.completed", map[string]any{"response": map[string]any{"id": "response-" + id, "status": "completed", "output": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	}))
	t.Cleanup(func() { unblock(); server.Close() })
	home := filepath.Join(f.root, "codex")
	if err := os.MkdirAll(filepath.Join(home, "memories"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "memories", "raw_memories.md"), []byte("Synthetic preference: concise English.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model_provider = \"probe\"\n[model_providers.probe]\nname = \"local\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\n[features]\nmemories = true\n[memories]\nconsolidation_model = \"gpt-5\"\nmin_rollout_idle_hours = 0\n", server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "bin", "codex"), []byte("#!/bin/sh\nexec "+shellQuote(codex)+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "profiles.author.kind", "codex")
	runPosse(t, f.binary, f.repo, f.env, "config", "set", "profiles.author.model", "gpt-5")
	if code, out := startReviewIdentityTask(t, f, "Memory author", "author", ""); code != 0 {
		t.Fatalf("start: %s", out)
	}
	close(started)
	f.waitTaskState(t, "t1", store.StateDone)
	author := f.mustTask(t, "t1")
	if code, out := startReviewIdentityTask(t, f, "Memory review", "reviewer", "t1"); code != 0 {
		t.Fatalf("review: %s", out)
	}
	review := f.mustTask(t, "t2")
	waitForReviewIdentityAgent(t, f, review.PaneID)
	targets, err := f.db.ReviewTargets(context.Background(), review.ID)
	if err != nil || len(targets) == 0 {
		t.Fatalf("review targets: %v %v", targets, err)
	}
	t.Logf("Before memory release: author=%s Review=%s targets=%#v", author.State, review.State, targets)
	unblock()
	select {
	case evidence := <-memoryFinished:
		t.Logf("Memory tool result: %s", evidence)
	case <-time.After(3 * time.Second):
	}
	if identity, err := os.ReadFile(filepath.Join(f.root, "memory-identity")); err == nil {
		t.Logf("Child shell: %s", identity)
	}
	if signal, err := os.ReadFile(filepath.Join(f.root, "memory-signal")); err == nil {
		t.Logf("Child Signal: %s", signal)
	}
	if state := f.mustTask(t, "t1").State; state != store.StateDone {
		transitions, err := f.db.TaskTransitions(context.Background(), author.ID, 20)
		t.Logf("Author transitions after memory response: %#v, error=%v", transitions, err)
		t.Fatalf("real Codex memory tool reopened completed author during Review: state=%s", state)
	}
	if state := f.mustTask(t, "t2").State; state != store.StateWorking {
		t.Fatalf("Review state=%s", state)
	}
	mu.Lock()
	count := memoryRequests
	mu.Unlock()
	if count != 0 {
		t.Fatalf("Codex generated %d background memory requests", count)
	}
	afterTargets, err := f.db.ReviewTargets(context.Background(), review.ID)
	if err != nil || fmt.Sprint(targets) != fmt.Sprint(afterTargets) {
		t.Fatalf("Review binding changed: %v %v", afterTargets, err)
	}
	if code, out := runReviewIdentityCLI(t, f, "send", "t1", "AUTHORIZED_FOLLOWUP: confirm the authorized follow-up and finish."); code != 0 {
		t.Fatalf("send: %s", out)
	}
	if !waitForCondition(20*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(f.root, "followup-signal"))
		return strings.Contains(string(b), "signal: done")
	}) {
		t.Fatal("authorized follow-up did not finish")
	}
	f.waitTaskState(t, "t1", store.StateDone)
	transitions, err := f.db.TaskTransitions(context.Background(), author.ID, 30)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tr := range transitions {
		if tr.Source == "lead" && tr.From == "done" && tr.To == "working" && tr.Note == "Lead submitted an instruction" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing authorized Lead transition: %#v", transitions)
	}
	signals, err := f.db.TaskSignals(context.Background(), author.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, signal := range signals {
		if signal.Verb == "working" && signal.Note == "Authorized Lead follow-up" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing authorized root Signal: %#v", signals)
	}
	t.Logf("Authorized follow-up transitions: %#v; Signals: %#v", transitions, signals)
}
