//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	posseRuntime "github.com/thanhbinh1905/posse/internal/runtime"
)

func TestRealPiInterruptStopsBusyTurnE2E(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("real Pi executable is not installed")
	}
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	piHome := filepath.Join(fixture.root, "pi")
	if err := os.MkdirAll(filepath.Join(piHome, "extensions"), 0o700); err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{}, 4)
	requestBodies := make(chan string, 4)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies <- string(body)
		requestStarted <- struct{}{}
		select {
		case <-r.Context().Done():
			return
		case <-release:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	defer server.Close()
	defer close(release)

	models := fmt.Sprintf(`{"providers":{"t265-local":{"baseUrl":%q,"api":"openai-responses","apiKey":"local-test-only","models":[{"id":"test","contextWindow":200000,"maxTokens":1024}]}}}`, server.URL)
	if err := os.WriteFile(filepath.Join(piHome, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piHome, "settings.json"), []byte(`{"retry":{"enabled":false},"showImages":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client := herdr.NewWithEnv("herdr", fixture.env)
	if _, err := client.Run(context.Background(), "integration", "install", "pi"); err != nil {
		t.Fatalf("install Pi integration in isolated Herdr: %v", err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.deep.kind", "pi")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "profiles.deep.model", "test")
	runPosse(t, fixture.binary, fixture.repo, fixture.env, "config", "set", "profiles.deep.args", `["--provider", "t265-local", "--no-skills", "--no-prompt-templates"]`)
	brief := filepath.Join(fixture.root, "real-pi-interrupt.md")
	briefText := "---\ntype: ship\ntitle: Real Pi interrupt\ndone_when: aborted turn observed\n---\nKeep working until the Lead interrupts this turn.\n"
	if err := os.WriteFile(brief, []byte(briefText), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "ride", "--brief", brief, "--name", "real-pi-interrupt")
	task := fixture.mustTask(t, "t1")
	select {
	case <-requestStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("real Pi did not start its local model request")
	}
	select {
	case <-requestBodies:
	case <-time.After(5 * time.Second):
		t.Fatal("local model request body was not captured")
	}
	if !waitForCondition(10*time.Second, func() bool {
		snapshot, err := client.Snapshot(context.Background())
		if err != nil {
			return false
		}
		for _, pane := range snapshot.Panes {
			if pane.PaneID == task.PaneID {
				return pane.Agent == "pi" && pane.AgentStatus == "working" && !pane.Focused
			}
		}
		return false
	}) {
		t.Fatalf("real Pi did not remain busy and unfocused before interrupt: %#v", currentPane(t, client, task.PaneID))
	}

	queued := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "Wait for the current turn", "--queue")
	if !strings.Contains(queued, "waiting_for_idle (--queue)") {
		t.Fatalf("real Pi queue head was not held: %s", queued)
	}
	redirect := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "send", "t1", "--interrupt", "Stop and redirect this turn")
	for _, expected := range []string{"state: delivered", "reason: delivered_after_interrupt", "interrupted: true", "verified: true"} {
		if !strings.Contains(redirect, expected) {
			t.Fatalf("real Pi send --interrupt omitted %q: %s", expected, redirect)
		}
	}
	pending, err := fixture.db.OldestQueuedMessage(context.Background(), task.ID)
	if err != nil || pending.Body != "Wait for the current turn" || !pending.WaitForIdle {
		t.Fatalf("send --interrupt reordered or consumed the queued message: %#v %v", pending, err)
	}
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("real Pi did not start a new model request for the redirect")
	}
	var redirectRequest string
	select {
	case redirectRequest = <-requestBodies:
	case <-time.After(5 * time.Second):
		t.Fatal("redirect model request body was not captured")
	}
	if !strings.Contains(redirectRequest, "Stop and redirect this turn") || strings.Contains(redirectRequest, "Wait for the current turn") {
		t.Fatalf("real Pi's next model input was not the redirect: %s", redirectRequest)
	}
	paneOutput, err := (posseRuntime.SystemProgress{}).ReadPane(context.Background(), client, task.PaneID, 160)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(paneOutput, "Operation aborted") {
		t.Fatalf("real Pi did not render its abort status: %s", paneOutput)
	}
}
