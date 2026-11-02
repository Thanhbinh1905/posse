package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestAttachRefusesPaneWhoseSessionDoesNotMatch(t *testing.T) {
	home, project, fake, service := newAttachTestService(t, "claude", "actual-session")
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &output

	if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "requested-session"}); code == 0 {
		t.Fatalf("attach accepted a different session: %s", output.String())
	}
	if !strings.Contains(output.String(), "attach_session_mismatch") {
		t.Fatalf("attach refusal did not identify the session mismatch: %s", output.String())
	}
	if fake.CallCount("pane.rename") != 0 || fake.CallCount("workspace.rename") != 0 || fake.CallCount("agent.prompt") != 0 {
		t.Fatalf("mismatched session changed Herdr state: %#v", fake.Calls)
	}
	assertAttachUnbound(t, home, project.ID)
}

func TestAttachRefusesFocusedPaneBeforeBinding(t *testing.T) {
	home, project, fake, service := newAttachTestService(t, "claude", "session-claude")
	fake.SnapshotValue.FocusedPaneID = "w1:p2"
	fake.SnapshotValue.Panes[1].Focused = true
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &output
	if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "session-claude"}); code == 0 || !strings.Contains(output.String(), "pane_focused") {
		t.Fatalf("attach did not refuse the focused pane: code=%d output=%s", code, output.String())
	}
	if fake.CallCount("agent.prompt") != 0 || fake.CallCount("pane.rename") != 0 {
		t.Fatalf("focused session was changed: %#v", fake.Calls)
	}
	assertAttachUnbound(t, home, project.ID)
}

func TestAttachRefusesBlockedAgentBeforeBinding(t *testing.T) {
	home, project, fake, service := newAttachTestService(t, "claude", "session-claude")
	fake.SnapshotValue.Panes[1].AgentStatus = "blocked"
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &output
	if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "session-claude"}); code == 0 || !strings.Contains(output.String(), "agent_blocked") {
		t.Fatalf("attach did not refuse the blocked pane: code=%d output=%s", code, output.String())
	}
	if fake.CallCount("agent.prompt") != 0 || fake.CallCount("pane.rename") != 0 {
		t.Fatalf("blocked session was changed: %#v", fake.Calls)
	}
	assertAttachUnbound(t, home, project.ID)
}

func TestAttachRefusesPaneWithoutLiveAgentBeforeBinding(t *testing.T) {
	for _, status := range []string{"", "unknown", "exited", "stopped"} {
		t.Run(status, func(t *testing.T) {
			home, project, fake, service := newAttachTestService(t, "claude", "session-claude")
			fake.SnapshotValue.Panes[1].AgentStatus = status
			var output bytes.Buffer
			cli := service.CLI()
			cli.Out = &output
			cli.ErrOut = &output
			if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "session-claude"}); code == 0 || !strings.Contains(output.String(), "attach_session_not_live") {
				t.Fatalf("attach accepted a pane without a live agent: status=%q code=%d output=%s", status, code, output.String())
			}
			if fake.CallCount("agent.prompt") != 0 || fake.CallCount("pane.rename") != 0 {
				t.Fatalf("non-live session was changed: %#v", fake.Calls)
			}
			assertAttachUnbound(t, home, project.ID)
		})
	}
}

func TestAttachRefusesChangedHerdrGenerationBeforePrompt(t *testing.T) {
	home, project, fake, service := newAttachTestService(t, "claude", "session-claude")
	adapter := &changingAttachGeneration{Fake: fake}
	service.Herdr = adapter
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &output
	if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "session-claude"}); code == 0 || !strings.Contains(output.String(), "attach_generation_changed") {
		t.Fatalf("attach did not refuse a generation change: code=%d output=%s", code, output.String())
	}
	if fake.CallCount("agent.prompt") != 0 || fake.CallCount("pane.rename") != 0 {
		t.Fatalf("stale generation changed the session: %#v", fake.Calls)
	}
	assertAttachUnbound(t, home, project.ID)
}

func TestAttachSupportsClaudeCodexAndPiSessions(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "pi"} {
		t.Run(kind, func(t *testing.T) {
			home, project, fake, service := newAttachTestService(t, kind, "session-"+kind)
			var output bytes.Buffer
			cli := service.CLI()
			cli.Out = &output
			cli.ErrOut = &output
			if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "session-" + kind}); code != 0 {
				t.Fatalf("attach failed: %s", output.String())
			}
			if fake.CallCount("agent.prompt") != 1 || fake.CallCount("agent.start") != 0 {
				t.Fatalf("attach did not preserve the existing session: %#v", fake.Calls)
			}
			if !attachedPromptTargets(fake, "w1:p2") || !attachedMetadataMarksFallback(fake) {
				t.Fatalf("attach did not target the verified pane and mark its fallback delivery mode: %#v", fake.Calls)
			}
			if !strings.Contains(output.String(), "attached") || !strings.Contains(output.String(), "session-"+kind) {
				t.Fatalf("attach omitted the verified session: %s", output.String())
			}
			db, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			attached, err := db.ProjectByID(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			lead, err := db.ProjectLeadAgent(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if attached.LeadPaneID != "w1:p2" || attached.HerdrWorkspaceID != "w1" || lead.Kind != kind || lead.Session != "session-"+kind {
				t.Fatalf("attached Lead identity = %#v, %#v", attached, lead)
			}
		})
	}
}

func TestAttachRefusesUnsupportedSessionBeforeBinding(t *testing.T) {
	home, project, fake, service := newAttachTestService(t, "opencode", "session-opencode")
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &output
	if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "session-opencode"}); code == 0 {
		t.Fatalf("attach accepted an unsupported session: %s", output.String())
	}
	if !strings.Contains(output.String(), "attach_unsupported_session") || !strings.Contains(output.String(), "posse up") {
		t.Fatalf("unsupported session refusal omitted the `posse up` suggestion: %s", output.String())
	}
	if fake.CallCount("agent.prompt") != 0 || fake.CallCount("pane.rename") != 0 || fake.CallCount("workspace.rename") != 0 {
		t.Fatalf("unsupported session was changed: %#v", fake.Calls)
	}
	assertAttachUnbound(t, home, project.ID)
}

func TestAttachKeepsOneLeadPerProject(t *testing.T) {
	home, project, fake, service := newAttachTestService(t, "claude", "session-claude")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p3", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p3", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"})
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &output
	if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "session-claude"}); code == 0 || !strings.Contains(output.String(), "lead_running") {
		t.Fatalf("attach did not preserve the one-Lead rule: code=%d output=%s", code, output.String())
	}
	if fake.CallCount("agent.prompt") != 0 || fake.CallCount("pane.rename") != 0 {
		t.Fatalf("second Lead session was changed: %#v", fake.Calls)
	}
}

func TestAttachRefusesForeignProjectLeadInTargetWorkspace(t *testing.T) {
	home, project, fake, service := newAttachTestService(t, "claude", "session-claude")
	fake.SnapshotValue.Panes = append(fake.SnapshotValue.Panes, herdr.Pane{
		PaneID: "w1:p3", WorkspaceID: "w1", Label: "posse:other:lead", Agent: "codex", AgentStatus: "idle",
	})
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &output
	if code := cli.Run([]string{"attach", "--pane", "w1:p2", "--session", "session-claude"}); code == 0 || !strings.Contains(output.String(), "lead_workspace_conflict") {
		t.Fatalf("attach did not preserve the one-Lead-per-workspace rule: code=%d output=%s", code, output.String())
	}
	if !strings.Contains(output.String(), "posse attach --pane <pane-id> --session <session-id>") || strings.Contains(output.String(), "posse up") {
		t.Fatalf("workspace refusal did not give attach-specific recovery guidance: %s", output.String())
	}
	if fake.CallCount("agent.prompt") != 0 || fake.CallCount("agent.start") != 0 {
		t.Fatalf("attach changed a session in a conflicting workspace: %#v", fake.Calls)
	}
	assertAttachUnbound(t, home, project.ID)
}

func newAttachTestService(t *testing.T, kind, sessionID string) (string, store.Project, *herdr.Fake, *Service) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[lead]\nkind = \""+kind+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	t.Chdir(repo)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		ServerStartedAt: "generation-1",
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: "w1"},
			{PaneID: "w1:p2", WorkspaceID: "w1", Agent: kind, AgentStatus: "idle", AgentSession: rawSession(kind, sessionID)},
		},
		Agents: []herdr.Agent{{PaneID: "w1:p2", Kind: kind, Agent: kind, SessionID: sessionID}},
	}
	return home, project, fake, testService(home, fake)
}

type changingAttachGeneration struct {
	*herdr.Fake
	snapshots int
}

func (a *changingAttachGeneration) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	a.snapshots++
	snapshot, err := a.Fake.Snapshot(ctx)
	if a.snapshots >= 3 {
		snapshot.ServerStartedAt = "generation-2"
	}
	return snapshot, err
}

func attachedPromptTargets(fake *herdr.Fake, paneID string) bool {
	for _, call := range fake.Calls {
		if call.Method == "agent.prompt" && call.Params["target"] == paneID {
			return true
		}
	}
	return false
}

func attachedMetadataMarksFallback(fake *herdr.Fake) bool {
	for _, call := range fake.Calls {
		if call.Method != "pane.report_metadata" {
			continue
		}
		tokens, ok := call.Params["tokens"].(map[string]string)
		return ok && tokens["posse_attached"] == "true"
	}
	return false
}

func rawSession(kind, sessionID string) json.RawMessage {
	if kind == "codex" {
		return json.RawMessage(`{"source":"herdr:codex","agent":"codex","kind":"id","value":"` + sessionID + `"}`)
	}
	data, _ := json.Marshal(map[string]string{"agent": kind, "session_id": sessionID})
	return data
}

func assertAttachUnbound(t *testing.T, home string, projectID int64) {
	t.Helper()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if project.LeadPaneID != "" {
		t.Fatalf("refused attachment was bound as the Lead: %#v", project)
	}
}
