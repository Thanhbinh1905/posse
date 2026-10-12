//go:build e2e

package e2e

import (
	"context"
	"fmt"
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
	requestStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requestStarted <- struct{}{}:
		default:
		}
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
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "profiles.deep.kind", "pi")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "profiles.deep.model", "test")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "config", "set", "profiles.deep.args", `["--provider", "t265-local", "--no-skills", "--no-prompt-templates"]`)
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

	output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "interrupt", "t1")
	if !strings.Contains(output, "interrupted: true") || !strings.Contains(output, "verified: true") {
		t.Fatalf("real Pi interrupt was not verified: %s", output)
	}
	paneOutput, err := (posseRuntime.SystemProgress{}).ReadPane(context.Background(), client, task.PaneID, 160)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(paneOutput, "Operation aborted") {
		t.Fatalf("real Pi did not render its abort status: %s", paneOutput)
	}
}
