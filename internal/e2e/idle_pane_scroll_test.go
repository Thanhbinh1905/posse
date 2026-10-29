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
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

// Herdr collects the history of an idle fullscreen agent by scrolling it to the
// top and back, so Posse's automatic reads must stay within the visible screen.
func TestIdleFullscreenWorkerKeepsItsViewDuringAutomaticReads(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("real Pi executable is not installed")
	}
	base := newPRLifecycleFixture(t)
	t.Cleanup(func() { _ = base.db.Close() })
	piHome := filepath.Join(base.root, "pi")
	if err := os.MkdirAll(filepath.Join(piHome, "extensions"), 0700); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": "This content was flagged for possible cybersecurity risk"}})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	}))
	t.Cleanup(api.Close)
	models := fmt.Sprintf(`{"providers":{"scroll-local":{"baseUrl":%q,"api":"openai-responses","apiKey":"dummy-local-only","models":[{"id":"test","contextWindow":200000,"maxTokens":1024}]}}}`, api.URL)
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
	runPosse(t, base.binary, base.repo, base.env, "config", "set", "profiles.deep.kind", "pi")
	runPosse(t, base.binary, base.repo, base.leadEnv, "config", "set", "profiles.deep.model", "test")
	runPosse(t, base.binary, base.repo, base.env, "config", "set", "profiles.deep.args", `["--provider", "scroll-local", "--no-skills", "--no-prompt-templates"]`)
	brief := filepath.Join(base.root, "idle-scroll.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Idle scroll\ndone_when: view kept\n---\nSay hello.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, base.binary, base.repo, base.leadEnv, "start", "--brief", brief, "--name", "idle-scroll")
	task := base.mustTask(t, "t1")

	visible := func() string {
		raw, err := client.Call(context.Background(), "pane.read", map[string]any{"pane_id": task.PaneID, "source": "visible"})
		if err != nil {
			return ""
		}
		var screen struct {
			Read struct {
				Text string `json:"text"`
			} `json:"read"`
		}
		_ = json.Unmarshal(raw, &screen)
		return screen.Read.Text
	}
	idle := func() bool {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID == task.PaneID {
				return pane.AgentStatus == "idle" || pane.AgentStatus == "done"
			}
		}
		return false
	}
	if !waitForCondition(20*time.Second, func() bool { return strings.Contains(visible(), "flagged") && idle() }) {
		t.Fatalf("Pi never finished its first turn:\n%s", visible())
	}
	// A transcript taller than the screen gives Herdr history to collect.
	if _, err := client.Call(context.Background(), "pane.send_input", map[string]any{"pane_id": task.PaneID, "text": "!seq 1 150", "keys": []string{"enter"}}); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(20*time.Second, func() bool { return strings.Contains(visible(), "ctrl+o to expand") && idle() }) {
		t.Fatalf("Pi never ran the shell command:\n%s", visible())
	}
	if _, err := client.Call(context.Background(), "pane.send_input", map[string]any{"pane_id": task.PaneID, "keys": []string{"ctrl+o"}}); err != nil {
		t.Fatal(err)
	}
	lastLine := regexp.MustCompile(`(?m)^\s*150\s*(│|┃)?$`)
	var settled string
	if !waitForCondition(20*time.Second, func() bool {
		screen := visible()
		if !lastLine.MatchString(screen) || strings.Contains(screen, "ctrl+o to expand") || !idle() || screen != settled {
			settled = screen
			return false
		}
		return true
	}) {
		t.Fatalf("Pi never settled after the shell command:\n%s", visible())
	}

	frames := map[string]bool{}
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			screen := visible()
			frames[screen] = true
			time.Sleep(20 * time.Millisecond)
		}
	}()
	// The turn-end event runs the model-error read and a reconcile with the quota and stall reads.
	event := fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"idle"}}`, task.PaneID)
	runPosse(t, base.binary, base.repo, setEnv(base.env, "HERDR_PLUGIN_EVENT_JSON", event), "_ingest")
	peeked := runPosse(t, base.binary, base.repo, base.leadEnv, "peek", "t1")
	time.Sleep(1500 * time.Millisecond)
	close(stop)
	<-sampled

	if len(frames) != 1 {
		var shown []string
		for frame := range frames {
			shown = append(shown, frame)
		}
		t.Fatalf("idle Worker view moved during automatic reads: %d frames\n%s", len(frames), strings.Join(shown, "\n=====\n"))
	}
	if !strings.Contains(peeked, " 150") {
		t.Fatalf("peek did not show the visible screen:\n%s", peeked)
	}
}
