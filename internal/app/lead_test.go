package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestConcurrentUpStartsAtMostOneLead(t *testing.T) {
	installFakeLeadAgents(t, "claude")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[lead]\nkind = \"claude\"\n\n[lead.profiles]\nclaude = \"reviewer\"\n\n[kinds.claude]\nsystem_prompt_args = [\"--append-system-prompt-file\", \"{file}\"]\nmodel_args = [\"--model={model}\"]\neffort_args = [\"--effort\", \"{effort}\"]\n\n[profiles.reviewer]\nkind = \"claude\"\nmodel = \"opus\"\neffort = \"high\"\nargs = [\"--profile-extra\"]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
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
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p1", Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "idle", Focused: true}}}
	fake.Results["tab.create"], _ = json.Marshal(map[string]any{
		"tab":       map[string]string{"tab_id": "w1:t2"},
		"root_pane": map[string]string{"pane_id": "w1:p2"},
	})
	fake.Results["agent.start"] = json.RawMessage(`{"agent":{"name":"posse-shop-lead-1"}}`)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	adapter := &blockedLeadStartAdapter{Fake: fake, entered: make(chan struct{}), release: make(chan struct{})}
	service := testService(home, adapter)
	var group sync.WaitGroup
	type result struct {
		code   int
		output string
	}
	outputs := make(chan result, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			var stdout, stderr bytes.Buffer
			cli := service.CLI()
			cli.Out = &stdout
			cli.ErrOut = &stderr
			code := cli.Run([]string{"up", "--name", "shop"})
			outputs <- result{code: code, output: stdout.String() + stderr.String()}
		}()
	}
	select {
	case <-adapter.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("first posse up did not reach tab creation")
	}
	var busy result
	select {
	case busy = <-outputs:
	case <-time.After(30 * time.Second):
		t.Fatal("second posse up did not return while the first Lead was starting")
	}
	if busy.code == 0 || !strings.Contains(busy.output, "lead_starting") {
		t.Fatalf("concurrent posse up did not report the start claim: %#v", busy)
	}
	close(adapter.release)
	group.Wait()
	created := <-outputs
	if created.code != 0 {
		t.Fatalf("Lead startup failed: %s", created.output)
	}
	if got := fake.CallCount("tab.create"); got != 1 {
		t.Fatalf("concurrent up created %d Lookout tabs, want one", got)
	}
	if got := fake.CallCount("agent.start"); got != 0 {
		t.Fatalf("concurrent up invoked Herdr agent.start %d times", got)
	}
	wantArgs := []string{"--append-system-prompt-file", filepath.Join(home, "projects", "shop", "lead.md"), "--plugin-dir", filepath.Join(home, "projects", "shop", "lead-claude-lowkey"), "--profile-extra", "--model=opus", "--effort", "high"}
	if service.pendingLead == nil || service.pendingLead.PaneID != "w1:p1" || !equalStrings(service.pendingLead.Args, wantArgs) {
		t.Fatalf("pending Lead launch = %#v, want pane w1:p1 and args %#v", service.pendingLead, wantArgs)
	}
	db, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if project.LeadPaneID != "w1:p1" || project.Status != "active" {
		t.Fatalf("recorded Lead = %#v", project)
	}
}

func TestUpRequiresLeadKindAndListsAvailableKinds(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"claude", "codex"} {
		if err := os.MkdirAll(filepath.Join(home, "."+kind), 0o700); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(bin, kind)
		if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	t.Chdir(repo)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p1", Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Focused: true}}}
	fake.Results["server.agent_manifests"] = json.RawMessage(`{"agents":[{"kind":"claude"},{"kind":"codex"}]}`)
	service := testService(filepath.Join(root, "posse"), fake)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	cli.ErrOut = &output
	if code := cli.Run([]string{"up", "--name", "shop"}); code == 0 {
		t.Fatalf("up selected an implicit Lead kind: %s", output.String())
	}
	for _, want := range []string{"lead_kind_required", "claude", "codex"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("up error %q does not include %q", output.String(), want)
		}
	}
}

func TestUpFocusesBusyLeadWithoutStartingAnotherAgent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[lead]\nkind = \"claude\"\n"), 0o600); err != nil {
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
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p2", "posse:shop:lead"); err != nil {
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
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "idle", Focused: true},
		{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"},
	}}
	service := testService(home, fake)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	if code := cli.Run([]string{"up", "--name", "shop"}); code != 1 || !strings.Contains(output.String(), "lead_running") || !strings.Contains(output.String(), "claude") || !strings.Contains(output.String(), "w1:p2") {
		t.Fatalf("up did not refuse the running Lead with its kind and pane: code=%d output=%s", code, output.String())
	}
	if fake.CallCount("agent.focus") != 0 || fake.CallCount("agent.start") != 0 || fake.CallCount("tab.create") != 0 || fake.CallCount("agent.get") != 0 {
		t.Fatalf("running Lead was changed: %#v", fake.Calls)
	}
}

func TestUpSchedulesProfileLeadPromptWithoutSystemPromptWhenNoticesArePending(t *testing.T) {
	installFakeLeadAgents(t, "claude")
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[lead]\nkind = \"claude\"\n\n[lead.profiles]\nclaude = \"lead-profile\"\n\n[kinds.claude]\nsystem_prompt_args = []\nmodel_args = [\"--model\", \"{model}\"]\n\n[profiles.lead-profile]\nkind = \"claude\"\nmodel = \"sonnet\"\nargs = [\"--profile-arg\"]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err == nil {
		_, err = db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "pending outcome", DataJSON: `{}`})
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	t.Chdir(repo)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "idle"}}}
	output := &bytes.Buffer{}
	service := testService(home, fake)
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"up", "--name", "shop"}); code != 0 {
		t.Fatalf("up failed: code=%d output=%s", code, output.String())
	}
	if service.pendingLead == nil || !service.pendingLead.NeedsPrompt || !equalStrings(service.pendingLead.Args, []string{"--plugin-dir", filepath.Join(home, "projects", "shop", "lead-claude-lowkey"), "--profile-arg", "--model", "sonnet"}) {
		t.Fatalf("profile Lead startup did not preserve its arguments and prompt requirement: %#v", service.pendingLead)
	}
	if fake.CallCount("agent.prompt") != 0 {
		t.Fatalf("Lead instructions were prompted before the agent took over the caller pane: %#v", fake.Calls)
	}
}

func TestUpClearsReplacedDeadLeadLabelAndIncrementsLaunchName(t *testing.T) {
	installFakeLeadAgents(t, "claude")
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[lead]\nkind = \"claude\"\n\n[kinds.claude]\nsystem_prompt_args = [\"--append-system-prompt-file\", \"{file}\"]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
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
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p2", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextLeadLaunch(ctx, project.ID); err != nil {
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
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "idle", Focused: true},
		{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:lead"},
		{PaneID: "w1:p3", WorkspaceID: "w1", Label: "posse:shop:lead"},
	}}
	fake.Results["tab.create"], _ = json.Marshal(map[string]any{
		"tab":       map[string]string{"tab_id": "w1:t3"},
		"root_pane": map[string]string{"pane_id": "w1:p3"},
	})
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	service := testService(home, fake)
	var output bytes.Buffer
	cli := service.CLI()
	cli.Out = &output
	if code := cli.Run([]string{"up", "--name", "shop"}); code != 0 || !strings.Contains(output.String(), "starting") {
		t.Fatalf("up did not replace the dead Lead: code=%d output=%s", code, output.String())
	}
	clearedDuplicateLabel, callerPaneSelected := false, false
	for _, call := range fake.Calls {
		if call.Method == "pane.rename" && call.Params["pane_id"] == "w1:p2" && call.Params["label"] == "" {
			clearedDuplicateLabel = true
		}
	}
	if service.pendingLead != nil && service.pendingLead.AgentName == "posse-shop-lead-2" && service.pendingLead.PaneID == "w1:p1" {
		callerPaneSelected = true
	}
	if !clearedDuplicateLabel || !callerPaneSelected || fake.CallCount("tab.create") != 1 {
		t.Fatalf("replacement did not use the caller pane and clear duplicate labels: %#v", fake.Calls)
	}
}

func TestUpReplaceStopsOldLeadAndUsesSelectedKindWithoutAutoApproval(t *testing.T) {
	installFakeLeadAgents(t, "codex")
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	configText := "[lead]\nkind = \"claude\"\n\n[lead.profiles]\ncodex = \"lead-codex\"\n\n[kinds.codex]\nsystem_prompt_args = [\"--system-prompt-file\", \"{file}\"]\nauto_approve_args = [\"--bypass\"]\n\n[profiles.lead-codex]\nkind = \"codex\"\nargs = [\"--lead-profile\"]\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
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
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p2", "posse:shop:lead"); err != nil {
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
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "idle", Focused: true},
		{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"},
	}}
	service := testService(home, &shellAfterLeadExitAdapter{Fake: fake})
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"up", "--replace", "--codex", "--name", "shop"}); code != 0 {
		t.Fatalf("up --replace --codex code=%d output=%s", code, output.String())
	}
	methods := []string{}
	for _, call := range fake.Calls {
		methods = append(methods, call.Method)
	}
	indices := []int{}
	for _, wanted := range []string{"agent.send_keys", "pane.rename"} {
		index := -1
		for current, method := range methods {
			if method == wanted {
				index = current
				break
			}
		}
		indices = append(indices, index)
	}
	if indices[0] < 0 || indices[1] <= indices[0] {
		t.Fatalf("replacement did not stop the old Lead before starting a new one: %#v", fake.Calls)
	}
	if service.pendingLead == nil || service.pendingLead.Kind != "codex" || service.pendingLead.PaneID != "w1:p1" || service.pendingLead.NeedsPrompt || !equalStrings(service.pendingLead.Args, []string{"--sandbox", "danger-full-access", "--system-prompt-file", filepath.Join(home, "projects", "shop", "lead.md"), "--lead-profile", codexOpeningPrompt}) {
		t.Fatalf("replacement kind/args/pane = %#v", service.pendingLead)
	}
}

func TestUpReplaceUsesLeadOwnershipInsteadOfStalePaneIDAndClearsLiveDuplicateLabel(t *testing.T) {
	installFakeLeadAgents(t, "claude")
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[lead]\nkind = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err == nil {
		err = db.SetProjectLead(ctx, project.ID, "w1", "w1:p2", "posse:shop:lead")
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	t.Chdir(repo)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{
		Panes: []herdr.Pane{
			{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "idle"},
			{PaneID: "w1:p2", WorkspaceID: "w1", Agent: "claude", AgentStatus: "working"},
			{PaneID: "w1:p3", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"},
			{PaneID: "w1:p4", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "working"},
		},
		Agents: []herdr.Agent{
			{Name: "posse-shop-lead-1", PaneID: "w1:p3", Kind: "claude"},
			{Name: "posse-shop-lead-2", PaneID: "w1:p4", Kind: "claude"},
		},
	}
	fake.Results["tab.create"], _ = json.Marshal(map[string]any{"tab": map[string]string{"tab_id": "w1:t5"}, "root_pane": map[string]string{"pane_id": "w1:p5"}})
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	output := &bytes.Buffer{}
	service := testService(home, &shellAfterLeadExitAdapter{Fake: fake})
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"up", "--name", "shop"}); code != 1 || !strings.Contains(output.String(), "lead_running") || !strings.Contains(output.String(), "claude") {
		t.Fatalf("named Lead agent without pane metadata was not identified: code=%d output=%s", code, output.String())
	}
	fake.Calls = nil
	output.Reset()
	if code := cli.Run([]string{"up", "--replace", "--name", "shop"}); code != 0 {
		t.Fatalf("up --replace failed: code=%d output=%s", code, output.String())
	}
	closedLeadPane, clearedLiveDuplicate, clearedLeadMetadata, stoppedLead := "", false, false, false
	for _, call := range fake.Calls {
		if call.Method == "pane.close" {
			closedLeadPane, _ = call.Params["pane_id"].(string)
		}
		if call.Method == "agent.send_keys" && (call.Params["target"] == "w1:p3" || call.Params["target"] == "w1:p4") {
			stoppedLead = true
		}
		if call.Method == "pane.rename" && call.Params["pane_id"] == "w1:p3" && call.Params["label"] == "" {
			clearedLiveDuplicate = true
		}
		if call.Method == "pane.report_metadata" && call.Params["pane_id"] == "w1:p3" && call.Params["clear_title"] == true && call.Params["clear_display_agent"] == true {
			clearedLeadMetadata = true
		}
		if call.Method == "pane.close" && call.Params["pane_id"] == "w1:p2" {
			t.Fatalf("replacement acted on an unrelated pane that reused the recorded Lead id: %#v", call)
		}
	}
	if closedLeadPane != "" || !stoppedLead || !clearedLiveDuplicate || !clearedLeadMetadata || service.pendingLead == nil || service.pendingLead.PaneID != "w1:p1" {
		t.Fatalf("replacement did not stop the named Lead while keeping its pane open and clearing its label and metadata: closed=%q stopped=%v duplicateCleared=%v metadataCleared=%v calls=%#v", closedLeadPane, stoppedLead, clearedLiveDuplicate, clearedLeadMetadata, fake.Calls)
	}
}

type shellAfterLeadExitAdapter struct {
	*herdr.Fake
	stopped bool
}

func (a *shellAfterLeadExitAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	result, err := a.Fake.Call(ctx, method, params)
	if method == "agent.send_keys" {
		a.stopped = true
	}
	return result, err
}

func (a *shellAfterLeadExitAdapter) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	snapshot, err := a.Fake.Snapshot(ctx)
	if err != nil || !a.stopped {
		return snapshot, err
	}
	for index := range snapshot.Panes {
		snapshot.Panes[index].Agent = ""
		snapshot.Panes[index].AgentStatus = "unknown"
	}
	return snapshot, nil
}

func TestUpAcceptsAtMostOneKindFlag(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	output := &bytes.Buffer{}
	cli := testService(t.TempDir(), nil).CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"up", "--claude", "--codex"}); code != 2 || !strings.Contains(output.String(), "one kind") {
		t.Fatalf("multiple Lead kind flags code=%d output=%s", code, output.String())
	}
}

func TestFinalizeLeadRenamesDetectedAgentAndDeliversFallbackInstructions(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[lead]\nkind = \"claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err == nil {
		err = db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead")
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")
	fastLaunchRetries(t)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{FocusedPaneID: "w1:p1", Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude", AgentStatus: "idle", Focused: true}}}
	fake.ErrorQueue["agent.prompt"] = []error{stalled(), nil}
	service := testService(home, fake)
	plan := leadExecPlan{ProjectID: project.ID, PaneID: "w1:p1", WorkspaceID: "w1", AgentName: "posse-shop-lead-1", Kind: "claude", NeedsPrompt: true}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.finalizeLead([]string{base64.RawURLEncoding.EncodeToString(payload)}); err != nil {
		t.Fatalf("finalize Lead: %v", err)
	}
	renamed, prompts := false, 0
	for _, call := range fake.Calls {
		if call.Method == "agent.rename" && call.Params["target"] == "w1:p1" && call.Params["name"] == "posse-shop-lead-1" {
			renamed = true
		}
		if call.Method == "agent.prompt" && call.Params["target"] == "w1:p1" && call.Params["text"] == "Run `posse lead` and follow it." && call.Params["wait"] != nil {
			prompts++
		}
	}
	// The focused calling pane still gets the prompt, resent after one dropped submission.
	if !renamed || prompts != 2 {
		t.Fatalf("finalizer did not track and initialize the Lead: %#v", fake.Calls)
	}
}

func TestLeadInstructionsReportNoticesProactivelyByKind(t *testing.T) {
	for kind, rule := range map[string]string{
		"claude": "Keep exactly one `posse lookout` running as a background command",
		"codex":  "posse queues each batch of Notices into this conversation",
		"pi":     "The Pi extension queues actionable Notices into model context as hidden Posse Notice messages",
		"gemini": "Notices arrive as a Posse Notice prompt only while your pane is idle and unfocused",
	} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			home := filepath.Join(root, "posse")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			configText := "[lead]\nkind = \"" + kind + "\"\n\n[kinds." + kind + "]\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.CreateProject(context.Background(), "shop", repo, "main"); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			var output bytes.Buffer
			cli := testService(home, nil).CLI()
			cli.Out = &output
			if code := cli.Run([]string{"lead"}); code != 0 {
				t.Fatalf("posse lead failed: %s", output.String())
			}
			for _, phrase := range []string{"When Posse delivers a Notice", "Run `posse`", "handle every Notice", "tell the User the outcome in your own words without waiting to be asked", "supervise Riders", "--name <short>", "then run `posse ack", "The optional Brief fields `issues:` and `refs:` are available for forge issue links", "`issues:` closes issues when the PR Lands", "`refs:` adds non-closing references", "qualify each issue number with its member name as `member#number`", "worker#12"} {
				if !strings.Contains(output.String(), phrase) {
					t.Fatalf("Lead instructions omitted %q: %s", phrase, output.String())
				}
			}
			if strings.Contains(output.String(), "record each affected issue in the Brief frontmatter") {
				t.Fatalf("Lead instructions prescribe issue links for every Task: %s", output.String())
			}
			if !strings.Contains(output.String(), rule) {
				t.Fatalf("%s Lead instructions omitted its Notice rule %q: %s", kind, rule, output.String())
			}
			if kind != "claude" && !strings.Contains(output.String(), "Do not run `posse lookout`") {
				t.Fatalf("%s Lead was not told to skip `posse lookout`: %s", kind, output.String())
			}
		})
	}
}

func installFakeLeadAgents(t *testing.T, kinds ...string) {
	t.Helper()
	binDir := t.TempDir()
	for _, kind := range kinds {
		binary := filepath.Join(binDir, kind)
		if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatalf("write fake %s executable: %v", kind, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type blockedLeadStartAdapter struct {
	*herdr.Fake
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockedLeadStartAdapter) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if method == "pane.rename" {
		a.once.Do(func() { close(a.entered) })
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return a.Fake.Call(ctx, method, params)
}

func TestWorkerProtocolScopesPRPublishing(t *testing.T) {
	brief := dispatch.Brief{Type: "ship", DoneWhen: "tests pass"}
	project := store.Project{Name: "shop"}
	local := workerProtocol(project, store.Task{Seq: 1, Type: "ship", LandingMode: "local"}, brief, "/tmp/launch.md")
	pr := workerProtocol(project, store.Task{Seq: 2, Type: "ship", LandingMode: "pr"}, brief, "/tmp/launch.md")
	noMistakes := workerProtocol(project, store.Task{Seq: 3, Type: "ship", LandingMode: "no-mistakes"}, brief, "/tmp/launch.md")
	if !strings.Contains(local, "Never git push or open a PR") {
		t.Fatalf("normal Worker protocol does not ban push: %s", local)
	}
	if !strings.Contains(pr, `posse publish "<summary>"`) || !strings.Contains(pr, "--pr <url>") || !strings.Contains(pr, "Never push directly") || !strings.Contains(pr, "or merge") {
		t.Fatalf("PR Worker protocol has incorrect publishing rules: %s", pr)
	}
	if strings.Contains(noMistakes, "git push") || !strings.Contains(noMistakes, "no-mistakes axi run --intent") {
		t.Fatalf("no-mistakes Worker protocol has incorrect delivery rules: %s", noMistakes)
	}
}
