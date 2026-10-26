package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// restoreFixture registers Project shop with a recorded Lead pane and a
// recorded Herdr generation, under a config whose lead.kind is pi.
func restoreFixture(t *testing.T) (string, *store.DB, store.Project) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[lead]\nkind = \"pi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "old-generation"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	return home, db, project
}

// claudeLeadSnapshot is the generation-old Herdr view of a running Claude Lead.
func claudeLeadSnapshot(project store.Project) herdr.Snapshot {
	return herdr.Snapshot{ServerStartedAt: "old-generation", Workspaces: []herdr.Workspace{{WorkspaceID: "w1"}}, Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel, Agent: "claude", AgentStatus: "working", AgentSession: []byte(`{"session_id":"claude-session-1"}`)}}}
}

// restartedSnapshot is Herdr after a restart: the Lead pane is a shell.
func restartedSnapshot(project store.Project) herdr.Snapshot {
	return herdr.Snapshot{ServerStartedAt: "new-generation", Workspaces: []herdr.Workspace{{WorkspaceID: "w1"}}, Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel}}}
}

func TestHerdrRestartDoesNotRelaunchLead(t *testing.T) {
	ctx := context.Background()
	home, db, project := restoreFixture(t)
	fake := herdr.NewFake()
	fake.SnapshotValue = claudeLeadSnapshot(project)
	service := testService(home, fake)
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.reconcileProject(ctx, db, project, cfg, false); err != nil {
		t.Fatal(err)
	}
	recorded, err := db.ProjectLeadAgent(ctx, project.ID)
	if err != nil || recorded != (store.LeadAgent{Kind: "claude", Session: "claude-session-1"}) {
		t.Fatalf("observed Lead = %#v, %v; want the running Claude Lead and its session", recorded, err)
	}
	fake.SnapshotValue = restartedSnapshot(project)
	for range 2 {
		if _, err := service.reconcileProject(ctx, db, project, cfg, false); !errors.Is(err, errRecoveryHeld) {
			t.Fatalf("reconcile after a Herdr restart = %v, want errRecoveryHeld", err)
		}
	}
	if count := fake.CallCount("agent.start"); count != 0 {
		t.Fatalf("Herdr restart relaunched %d agents automatically: %#v", count, fake.Calls)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	restoreNotices := 0
	for _, notice := range notices {
		if notice.Kind == "restore_needed" && strings.Contains(notice.Summary, "posse up --all") {
			restoreNotices++
		}
	}
	if restoreNotices != 1 {
		t.Fatalf("restore_needed Notices = %d, want one: %#v", restoreNotices, notices)
	}
	if fake.CallCount("notification.show") != 1 {
		t.Fatalf("the User was not notified once through Herdr: %#v", fake.Calls)
	}
	if generation, err := db.ProjectServerStartedAt(ctx, project.ID); err != nil || generation != "old-generation" {
		t.Fatalf("held restart consumed the generation: %q, %v", generation, err)
	}
}

func TestUpAllRestoresRecordedClaudeLeadOverLeadKind(t *testing.T) {
	ctx := context.Background()
	home, db, project := restoreFixture(t)
	if err := db.ObserveProjectLeadAgent(ctx, project.ID, store.LeadAgent{Kind: "claude", Session: "claude-session-1"}); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = restartedSnapshot(project)
	fake.Results["agent.get"] = []byte(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	fake.BeforeCall = func(method string) {
		if method == "agent.start" {
			fake.SnapshotValue.Panes[0].Agent, fake.SnapshotValue.Panes[0].AgentStatus = "claude", "idle"
		}
	}
	service := testService(home, fake)
	if err := service.holdRestore(ctx, db, project, "new-generation"); !errors.Is(err, errRecoveryHeld) {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	t.Setenv("HERDR_PANE_ID", "")
	if code := cli.Run([]string{"up", "--all"}); code != 0 || !strings.Contains(output.String(), "state: restored") {
		t.Fatalf("up --all = %d: %s", code, output)
	}
	if pending, err := db.UndeliveredNotices(ctx, project.ID); err != nil || len(pending) != 0 {
		t.Fatalf("restore left its restore_needed Notice for the restored Lead: %#v, %v", pending, err)
	}
	var starts []herdr.CallRecord
	for _, call := range fake.Calls {
		if call.Method == "agent.start" {
			starts = append(starts, call)
		}
	}
	if len(starts) != 1 {
		t.Fatalf("up --all started %d Leads, want one: %#v", len(starts), fake.Calls)
	}
	args := starts[0].Params["args"].([]string)
	if starts[0].Params["kind"] != "claude" || !equalStrings(args[len(args)-2:], []string{"--resume", "claude-session-1"}) {
		t.Fatalf("restored Lead = %v %#v; want Claude resuming its recorded session while lead.kind=pi", starts[0].Params["kind"], args)
	}
	if generation, err := db.ProjectServerStartedAt(ctx, project.ID); err != nil || generation != "new-generation" {
		t.Fatalf("restore left the generation pending: %q, %v", generation, err)
	}
	// A second restore finds the Lead running and starts nothing.
	output.Reset()
	if code := cli.Run([]string{"up", "--all"}); code != 0 || fake.CallCount("agent.start") != 1 {
		t.Fatalf("repeated up --all = %d, agent.start calls %d: %s", code, fake.CallCount("agent.start"), output)
	}
}

func TestUpAllSkipsDownProjects(t *testing.T) {
	ctx := context.Background()
	home, db, project := restoreFixture(t)
	if err := db.MarkProjectDown(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = restartedSnapshot(project)
	output := &bytes.Buffer{}
	cli := testService(home, fake).CLI()
	cli.Out, cli.ErrOut = output, output
	t.Setenv("HERDR_PANE_ID", "")
	if code := cli.Run([]string{"up", "--all"}); code != 0 || !strings.Contains(output.String(), "down_projects[1]: shop") || fake.CallCount("agent.start") != 0 {
		t.Fatalf("up --all touched a down Project: code=%d %s", code, output)
	}
}

func TestUpRefusalNamesTheRunningLeadAgent(t *testing.T) {
	home, _, project := restoreFixture(t)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	t.Chdir(project.Root)
	fake := herdr.NewFake()
	// Herdr's registry still names the kind Posse started; detection shows
	// the agent actually running in the Lead pane.
	fake.SnapshotValue = herdr.Snapshot{ServerStartedAt: "old-generation", Workspaces: []herdr.Workspace{{WorkspaceID: "w1"}}, Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: project.LeadLabel, Agent: "claude", AgentStatus: "working"},
		{PaneID: "w1:p2", WorkspaceID: "w1", AgentStatus: "idle"},
	}, Agents: []herdr.Agent{{Name: "posse-shop-lead-1", PaneID: "w1:p1", Kind: "pi"}}}
	output := &bytes.Buffer{}
	cli := testService(home, fake).CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"up", "--claude"}); code != 1 || !strings.Contains(output.String(), "already has a claude Lead running in pane w1:p1") {
		t.Fatalf("up refusal did not name the running Claude Lead: code=%d %s", code, output)
	}
}

func TestLeadResumeKeepsCodexOpeningPromptLast(t *testing.T) {
	launch := leadLaunch{Args: []string{"--sandbox", "danger-full-access", codexOpeningPrompt}, OpeningPromptArg: true}
	got := launch.withResume(config.Kind{ResumeArgs: []string{"resume", "{session}"}}, "thread-1").Args
	if !equalStrings(got, []string{"--sandbox", "danger-full-access", "resume", "thread-1", codexOpeningPrompt}) {
		t.Fatalf("Codex resume argv = %#v", got)
	}
	if got := launch.withResume(config.Kind{ResumeArgs: []string{"resume", "{session}"}}, "").Args; !equalStrings(got, launch.Args) {
		t.Fatalf("no session changed argv: %#v", got)
	}
}
