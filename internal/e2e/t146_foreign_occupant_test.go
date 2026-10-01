//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/app"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// All operations except the single preflight readiness failure use the real
// isolated Herdr server. Replace the original Rider with a separately named
// agent through public Herdr commands, retaining the reused pane's cwd.
type t146ReadinessAdapter struct {
	*herdr.Client
	lead     string
	failures int
}

func (a *t146ReadinessAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "agent.prompt" && params["target"] == a.lead {
		a.failures++
		return nil, &herdr.Error{Code: "agent_not_ready", Message: "agent is no longer the pane foreground process"}
	}
	return a.Client.Call(ctx, method, params)
}

func TestT146RealHerdrForeignAgentInReusedPaneSurvivesDiscard(t *testing.T) {
	f := newRiderTabsFixture(t)
	ctx := context.Background()
	task := f.ride(t, "t1", "Foreign occupant", "foreign-occupant")
	f.fail(t, "t1")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	_, noticeErr := db.CreateNotice(ctx, store.Notice{ProjectID: f.projectID, TaskID: task.ID, Kind: "worker_failed", Summary: "Rider failed", DataJSON: `{}`})
	_ = db.Close()
	if noticeErr != nil {
		t.Fatal(noticeErr)
	}
	configPath := filepath.Join(f.home, "config.toml")
	projectConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	projectConfig = append(projectConfig, []byte("\n[kinds.claude]\nnotice_delivery = \"prompt\"\n")...)
	if err := os.WriteFile(configPath, projectConfig, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Run(ctx, "agent", "send-keys", task.PaneID, "ctrl+c"); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(10*time.Second, func() bool {
		for _, pane := range f.snapshot(t).Panes {
			if pane.PaneID == task.PaneID {
				return pane.Agent == ""
			}
		}
		return false
	}) {
		t.Fatal("original Rider did not return to its shell")
	}
	if _, err := f.client.Call(ctx, "pane.rename", map[string]any{"pane_id": task.PaneID, "label": "foreign-agent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Run(ctx, "agent", "start", "foreign-agent", "--kind", "claude", "--pane", task.PaneID); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot(t)
	foreign := false
	for _, agent := range before.Agents {
		if agent.PaneID == task.PaneID && agent.Name == "foreign-agent" {
			foreign = true
		}
	}
	if !foreign {
		t.Fatalf("replacement agent was not recognized: %#v", before.Agents)
	}
	work := filepath.Join(task.WorktreePath, "foreign-agent-work.txt")
	if err := os.WriteFile(work, []byte("foreign work must survive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	processBefore, err := f.client.Call(ctx, "pane.process_info", map[string]any{"pane_id": task.PaneID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Run(ctx, "pane", "report-agent", f.leadPaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Call(ctx, "pane.focus", map[string]any{"pane_id": f.userBefore.RootPane.PaneID}); err != nil {
		t.Fatal(err)
	}
	preflight := f.snapshot(t)
	leadReady := false
	for _, pane := range preflight.Panes {
		if pane.PaneID == f.leadPaneID {
			leadReady = (pane.AgentStatus == "idle" || pane.AgentStatus == "done") && !pane.Focused && preflight.FocusedPaneID != f.leadPaneID
		}
	}
	if !leadReady {
		t.Fatalf("preflight setup did not leave an idle Lead unfocused: snapshot=%#v", preflight)
	}
	for _, entry := range f.leadEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			t.Setenv(key, value)
		}
	}
	t.Chdir(f.repo)
	adapter := &t146ReadinessAdapter{Client: f.client, lead: f.leadPaneID}
	service := app.New(f.home, adapter)
	cli := service.CLI()
	var output bytes.Buffer
	cli.Out, cli.ErrOut = &output, &output
	code := cli.Run([]string{"unsaddle", "t1", "--discard", "--user-approved", "User approved this discard"})
	if adapter.failures != 1 {
		t.Fatalf("readiness race boundary not exercised: failures=%d exit=%d output=%s", adapter.failures, code, output.String())
	}
	if code == 0 || !strings.Contains(output.String(), "unsaddle_incomplete") {
		t.Fatalf("unproven foreign foreground process was not refused with a typed Teardown error: exit=%d output=%s", code, output.String())
	}
	contents, fileErr := os.ReadFile(work)
	after := f.snapshot(t)
	foreignStillRunning := false
	for _, agent := range after.Agents {
		if agent.PaneID == task.PaneID && agent.Name == "foreign-agent" {
			foreignStillRunning = true
		}
	}
	t.Logf("before foreground=%s; discard exit=%d output=%s; after agents=%#v; foreign file error=%v", processBefore, code, output.String(), after.Agents, fileErr)
	if !foreignStillRunning || fileErr != nil || string(contents) != "foreign work must survive\n" {
		t.Fatalf("FOREIGN OCCUPANT DAMAGED: running=%v file=%q error=%v", foreignStillRunning, contents, fileErr)
	}
	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	updated, err := db.Task(ctx, f.projectID, "t1")
	if err != nil || updated.State != store.StateFailed {
		t.Fatalf("Task after refused Teardown = %#v, %v", updated, err)
	}
	mounts, err := db.Mounts(ctx, f.projectID)
	if err != nil || len(mounts) != 1 || mounts[0].State != "held" {
		t.Fatalf("Mount after refused Teardown = %#v, %v", mounts, err)
	}
}
