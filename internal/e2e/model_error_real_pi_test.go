//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	posseRuntime "github.com/thanhbinh1905/posse/internal/runtime"
)

// The model endpoint is local, uses a dummy credential, and never contacts a provider.
func TestT242RealPiErrorUIIsRecognized(t *testing.T) {
	t242RealPiErrorUI(t, "stream error: stream disconnected before completion: stream closed before response.completed", "model_stream_error")
}

func TestT242RealPiRefusalUIIsNotRetried(t *testing.T) {
	t242RealPiErrorUI(t, "This content was flagged for possible cybersecurity risk", "model_refused")
}

func t242RealPiErrorUI(t *testing.T, message, expectedKind string) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("real Pi executable is not installed")
	}
	base := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = base.db.Close() })
	piHome := filepath.Join(base.root, "pi")
	if err := os.MkdirAll(filepath.Join(piHome, "extensions"), 0700); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseModel := func() { releaseOnce.Do(func() { close(release) }) }
	var requests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(api.Close)
	t.Cleanup(releaseModel)
	models := fmt.Sprintf(`{"providers":{"t242-local":{"baseUrl":%q,"api":"openai-responses","apiKey":"dummy-local-only","models":[{"id":"test","contextWindow":200000,"maxTokens":1024}]}}}`, api.URL)
	if err := os.WriteFile(filepath.Join(piHome, "models.json"), []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piHome, "settings.json"), []byte(`{"retry":{"enabled":false},"showImages":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", base.env)
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatal(err)
	}
	runPosse(t, base.binary, base.repo, base.leadEnv, "config", "set", "profiles.deep.kind", "pi")
	runPosse(t, base.binary, base.repo, base.leadEnv, "config", "set", "profiles.deep.model", "test")
	runPosse(t, base.binary, base.repo, base.leadEnv, "config", "set", "profiles.deep.args", `["--provider", "t242-local", "--no-skills", "--no-prompt-templates"]`)
	brief := filepath.Join(base.root, "real-model-error.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Real Pi stream error\ndone_when: error handled\n---\nSay hello.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// PATH resolves the real Pi executable; its agent directory and provider are isolated.
	runPosse(t, base.binary, base.repo, base.leadEnv, "ride", "--brief", brief, "--name", "real-pi-stream")
	task := base.mustTask(t, "t1")
	releaseModel()
	var output string
	if !waitForCondition(10*time.Second, func() bool {
		output, _ = (posseRuntime.SystemProgress{}).ReadPane(context.Background(), client, task.PaneID, 200)
		return strings.Contains(output, message)
	}) {
		t.Fatalf("real Pi never rendered model error: %s", output)
	}
	time.Sleep(500 * time.Millisecond)
	output, err := (posseRuntime.SystemProgress{}).ReadPane(context.Background(), client, task.PaneID, 200)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real Pi pane.read output:\n%s", output)
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == task.PaneID {
			t.Logf("real Pi status: agent=%s state=%s focused=%t", pane.Agent, pane.AgentStatus, pane.Focused)
		}
	}
	// Force only the hook, retaining the real agent and its real terminal rendering.
	event := fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"idle"}}`, task.PaneID)
	env := setEnv(base.env, "HERDR_PLUGIN_EVENT_JSON", event)
	runPosse(t, base.binary, base.repo, env, "_ingest")
	episode, err := base.db.TaskModelErrorEpisode(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(episode)
	if episode.Kind != expectedKind {
		t.Fatalf("real Pi error UI not recognized: want=%s episode=%s", expectedKind, encoded)
	}
	if expectedKind == "model_refused" {
		time.Sleep(700 * time.Millisecond)
		if n := requests.Load(); n != 1 {
			t.Fatalf("refusal retried: requests=%d", n)
		}
		if episode.Status != "refused" || episode.Attempts != 0 {
			t.Fatalf("refusal episode=%s", encoded)
		}
	}
}
