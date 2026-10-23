package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func fastLaunchRetries(t *testing.T) {
	t.Helper()
	promptDelay, startDelay, startWait, readyWait := launchPromptRetryDelay, agentStartRetryDelay, agentStartBusyWait, agentReadyWait
	launchPromptRetryDelay, agentStartRetryDelay, agentStartBusyWait, agentReadyWait = 0, 0, 50*time.Millisecond, 600*time.Millisecond
	t.Cleanup(func() {
		launchPromptRetryDelay, agentStartRetryDelay, agentStartBusyWait, agentReadyWait = promptDelay, startDelay, startWait, readyWait
	})
}

func promptDeliveryService(t *testing.T) (*Service, *herdr.Fake) {
	t.Helper()
	fastLaunchRetries(t)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "codex", AgentStatus: "idle"}}}
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	return testService(filepath.Join(t.TempDir(), "posse"), fake), fake
}

func stalled() error {
	return &herdr.Error{Code: "agent_prompt_stalled", Message: "no agent activity was observed after the prompt"}
}

func TestLaunchPromptResendsWhenAgentDropsFirstSubmission(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.ErrorQueue["agent.prompt"] = []error{stalled(), nil}
	if err := service.deliverLaunchPrompt(context.Background(), "w1:p1", "Read launch.md and follow it."); err != nil {
		t.Fatalf("deliverLaunchPrompt: %v", err)
	}
	if got := fake.CallCount("agent.prompt"); got != 2 {
		t.Fatalf("prompt calls = %d, want 2", got)
	}
	for _, call := range fake.Calls {
		if call.Method != "agent.prompt" {
			continue
		}
		wait, ok := call.Params["wait"].(map[string]any)
		if !ok || !equalStrings(wait["until"].([]string), []string{"working", "blocked"}) {
			t.Fatalf("launch prompt does not wait for agent activity: %#v", call.Params)
		}
	}
}

func TestLaunchPromptPressesEnterWhenStalledTextIsStillInTheInput(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.ErrorQueue["agent.prompt"] = []error{stalled()}
	fake.Results["pane.read"] = json.RawMessage(`{"read":{"text":" pi v0.87.0\n Update Available\n────\n Read /home/u/.posse/projects/shop/tasks/t1/\n launch.md and follow it.\n────\n"}}`)
	if err := service.deliverLaunchPrompt(context.Background(), "w1:p1", "Read /home/u/.posse/projects/shop/tasks/t1/launch.md and follow it."); err != nil {
		t.Fatalf("deliverLaunchPrompt: %v", err)
	}
	if got := fake.CallCount("agent.prompt"); got != 1 {
		t.Fatalf("prompt calls = %d, want 1 so the text is not typed twice", got)
	}
	var keys, waits []map[string]any
	for _, call := range fake.Calls {
		switch call.Method {
		case "agent.send_keys":
			keys = append(keys, call.Params)
		case "agent.wait":
			waits = append(waits, call.Params)
		}
	}
	if len(keys) != 1 || !equalStrings(keys[0]["keys"].([]string), []string{"enter"}) {
		t.Fatalf("send_keys calls = %#v, want one enter", keys)
	}
	if len(waits) != 1 || !equalStrings(waits[0]["until"].([]string), []string{"working", "blocked"}) {
		t.Fatalf("wait calls = %#v, want one wait for activity", waits)
	}
}

func TestLaunchPromptRetypesWhenPressingEnterDoesNotStartTheAgent(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.ErrorQueue["agent.prompt"] = []error{stalled(), nil}
	fake.Errors["agent.wait"] = &herdr.Error{Code: "timeout", Message: "timed out waiting for agent status"}
	fake.Results["pane.read"] = json.RawMessage(`{"read":{"text":"$ Read launch.md and follow it.\n"}}`)
	if err := service.deliverLaunchPrompt(context.Background(), "w1:p1", "Read launch.md and follow it."); err != nil {
		t.Fatalf("deliverLaunchPrompt: %v", err)
	}
	if got := fake.CallCount("agent.send_keys"); got != 1 {
		t.Fatalf("send_keys calls = %d, want 1", got)
	}
	if got := fake.CallCount("agent.prompt"); got != 2 {
		t.Fatalf("prompt calls = %d, want 2", got)
	}
}

func TestLaunchPromptDoesNotPressEnterInAFocusedPane(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.ErrorQueue["agent.prompt"] = []error{stalled()}
	fake.Results["pane.read"] = json.RawMessage(`{"read":{"text":"Read launch.md and follow it."}}`)
	fake.BeforeCall = func(method string) {
		if method == "pane.read" {
			fake.SnapshotValue.FocusedPaneID = "w1:p1"
		}
	}
	err := service.deliverLaunchPrompt(context.Background(), "w1:p1", "Read launch.md and follow it.")
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "pane_focused" {
		t.Fatalf("focused pane error = %v", err)
	}
	if got := fake.CallCount("agent.send_keys"); got != 0 {
		t.Fatalf("send_keys calls = %d, want 0", got)
	}
}

func TestLaunchPromptFailsWhenAgentNeverStartsWorking(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.Errors["agent.prompt"] = stalled()
	err := service.deliverLaunchPrompt(context.Background(), "w1:p1", "Read launch.md and follow it.")
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "prompt_not_delivered" {
		t.Fatalf("undelivered prompt error = %v", err)
	}
	if got := fake.CallCount("agent.prompt"); got != launchPromptAttempts {
		t.Fatalf("prompt calls = %d, want %d", got, launchPromptAttempts)
	}
}

func TestLaunchPromptDoesNotResendWhenAgentIsAlreadyWorking(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.Errors["agent.prompt"] = stalled()
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"working"}}`)
	if err := service.deliverLaunchPrompt(context.Background(), "w1:p1", "Read launch.md and follow it."); err != nil {
		t.Fatalf("deliverLaunchPrompt: %v", err)
	}
	if got := fake.CallCount("agent.prompt"); got != 1 {
		t.Fatalf("prompt calls = %d, want 1", got)
	}
}

func TestLaunchPromptReturnsNonStallErrorsWithoutRetry(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.Errors["agent.prompt"] = &herdr.Error{Code: "agent_blocked", Message: "agent is waiting at a dialog"}
	err := service.deliverLaunchPrompt(context.Background(), "w1:p1", "Read launch.md and follow it.")
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "agent_blocked" {
		t.Fatalf("blocked prompt error = %v", err)
	}
	if got := fake.CallCount("agent.prompt"); got != 1 {
		t.Fatalf("prompt calls = %d, want 1", got)
	}
}

type startFixture struct {
	fake    *herdr.Fake
	service *Service
	home    string
	repo    string
	brief   string
	project store.Project
}

func newStartFixture(t *testing.T) startFixture {
	t.Helper()
	fastLaunchRetries(t)
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse")
	if err := os.MkdirAll(filepath.Join(home, "projects", "shop"), 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := "[defaults]\nmerge_mode = \"local\"\n\n[profiles.claude]\nkind = \"claude\"\n\n[dispatch.default]\nuse = \"claude\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(globalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(root, "brief.md")
	if err := os.WriteFile(briefPath, []byte("---\ntype: change\ntitle: Launch probe\ndone_when: the Worker acts\n---\nCreate a committed file.\n"), 0o600); err != nil {
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
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	fake.Results["tab.create"] = json.RawMessage(`{"tab":{"tab_id":"w1:t2","workspace_id":"w1"},"root_pane":{"pane_id":"w1:p2","tab_id":"w1:t2"}}`)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"idle","interactive_ready":true,"launch_pending":false}}`)
	t.Chdir(repo)
	return startFixture{fake: fake, service: testService(home, fake), home: home, repo: repo, brief: briefPath, project: project}
}

func (f startFixture) start(t *testing.T) (int, string) {
	t.Helper()
	output := &bytes.Buffer{}
	cli := f.service.CLI()
	cli.Out, cli.ErrOut = output, output
	code := cli.Run([]string{"start", "--brief", f.brief, "--name", "launch-probe"})
	return code, output.String()
}

func (f startFixture) onlyTask(t *testing.T) (store.Task, []store.Notice) {
	t.Helper()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.Tasks(context.Background(), f.project.ID, true)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("Tasks = %#v, %v", tasks, err)
	}
	notices, err := db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	return tasks[0], notices
}

func TestStartRejectsMultipleClosingIssuesBeforeAcquiringSandbox(t *testing.T) {
	fixture := newStartFixture(t)
	brief := "---\ntype: change\ntitle: Launch probe\ndone_when: the Worker acts\nissues: [12, 13]\n---\nCreate a committed file.\n"
	if err := os.WriteFile(fixture.brief, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output := fixture.start(t)
	if code == 0 || !strings.Contains(output, "issues") || !strings.Contains(output, "only one closing issue") {
		t.Fatalf("start accepted multiple closing issues: code=%d output=%s", code, output)
	}
	if fixture.fake.CallCount("tab.create") != 0 || fixture.fake.CallCount("agent.start") != 0 {
		t.Fatalf("invalid Brief acquired Worker resources: calls=%#v", fixture.fake.Calls)
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if tasks, err := db.Tasks(context.Background(), fixture.project.ID, true); err != nil || len(tasks) != 0 {
		t.Fatalf("invalid Brief created Tasks: %#v, %v", tasks, err)
	}
	if sandboxes, err := db.Sandboxes(context.Background(), fixture.project.ID); err != nil || len(sandboxes) != 0 {
		t.Fatalf("invalid Brief acquired Sandboxes: %#v, %v", sandboxes, err)
	}
}

func TestSupportedLaunchKindsDeliverBriefWithoutWaitingForFocus(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "pi", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newStartFixture(t)
			cfg := "[defaults]\nmerge_mode = \"local\"\n\n[profiles." + kind + "]\nkind = \"" + kind + "\"\n\n[dispatch.default]\nuse = \"" + kind + "\"\n"
			if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			fixture.fake.BeforeCall = func(method string) {
				if method == "agent.start" {
					fixture.fake.SnapshotValue.FocusedPaneID = "w1:p2"
					fixture.fake.SnapshotValue.Panes = append(fixture.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", TabID: "w1:t2", Label: "posse:shop:t1", Agent: kind, AgentStatus: "working", Focused: true})
				}
			}
			if code, output := fixture.start(t); code != 0 || !strings.Contains(output, "state: working") {
				t.Fatalf("%s launch: %d %s", kind, code, output)
			}
			openingPrompt := kind == "codex" || kind == "opencode"
			wantPromptCalls := 1
			if openingPrompt {
				wantPromptCalls = 0
			}
			if got := fixture.fake.CallCount("agent.prompt"); got != wantPromptCalls {
				t.Fatalf("%s prompt calls = %d, want %d", kind, got, wantPromptCalls)
			}
			var startArgs []string
			var typedPrompt string
			for _, call := range fixture.fake.Calls {
				switch call.Method {
				case "agent.start":
					startArgs = call.Params["args"].([]string)
				case "agent.prompt":
					typedPrompt, _ = call.Params["text"].(string)
				}
			}
			containsLaunchPath := strings.Contains(strings.Join(startArgs, " "), "launch.md")
			if containsLaunchPath != openingPrompt {
				t.Fatalf("%s opening args=%q, want launch path=%t", kind, startArgs, openingPrompt)
			}
			if !openingPrompt && !strings.Contains(typedPrompt, "launch.md") {
				t.Fatalf("%s typed launch prompt=%q", kind, typedPrompt)
			}
		})
	}
}

func TestStartFocusedDuringLaunchCompletesWithTypedPrompt(t *testing.T) {
	fixture := newStartFixture(t)
	fixture.fake.BeforeCall = func(method string) {
		if method == "agent.start" {
			fixture.fake.SnapshotValue.FocusedPaneID = "w1:p2"
			fixture.fake.SnapshotValue.FocusedWorkspaceID = "w1"
			fixture.fake.SnapshotValue.Panes = append(fixture.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", TabID: "w1:t2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle", Focused: true})
		}
	}
	code, output := fixture.start(t)
	task, notices := fixture.onlyTask(t)
	if code != 0 || task.State != store.StateWorking || len(notices) != 0 || fixture.fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("focused start code=%d output=%s task=%#v notices=%#v prompts=%d", code, output, task, notices, fixture.fake.CallCount("agent.prompt"))
	}
}

func TestFocusedLaunchNeedsNoFocusRetryOrNotice(t *testing.T) {
	fixture := newStartFixture(t)
	fixture.fake.BeforeCall = func(method string) {
		if method == "agent.start" {
			fixture.fake.SnapshotValue.FocusedPaneID = "w1:p2"
			fixture.fake.SnapshotValue.FocusedWorkspaceID = "w1"
			fixture.fake.SnapshotValue.Panes = append(fixture.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", TabID: "w1:t2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle", Focused: true})
		}
	}
	if code, output := fixture.start(t); code != 0 {
		t.Fatalf("start: %d %s", code, output)
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	task, _ := fixture.onlyTask(t)
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET created_at=? WHERE id=?`, time.Now().Add(-launchDeliveryTimeout-time.Second).UnixMilli(), task.ID); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := fixture.service.retryPendingLaunches(ctx, db, fixture.project, cfg, fixture.fake.SnapshotValue); err != nil {
			t.Fatal(err)
		}
	}
	task, notices := fixture.onlyTask(t)
	if task.State != store.StateWorking || len(notices) != 0 || fixture.fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("focused launch task=%#v notices=%#v prompts=%d", task, notices, fixture.fake.CallCount("agent.prompt"))
	}
	fixture.fake.SnapshotValue.FocusedPaneID = "w1:p1"
	fixture.fake.SnapshotValue.FocusedWorkspaceID = "w1"
	fixture.fake.SnapshotValue.Panes[1].Focused = false
	if err := fixture.service.retryPendingLaunches(ctx, db, fixture.project, cfg, fixture.fake.SnapshotValue); err != nil {
		t.Fatal(err)
	}
	task, _ = fixture.onlyTask(t)
	if task.State != store.StateWorking || fixture.fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("focus reconciliation changed launch task=%#v prompts=%d", task, fixture.fake.CallCount("agent.prompt"))
	}
}

func TestFocusedStartSubmitsLaunchPromptWithoutRetryNotice(t *testing.T) {
	fixture := newStartFixture(t)
	fixture.fake.BeforeCall = func(method string) {
		if method == "agent.start" {
			fixture.fake.SnapshotValue.FocusedPaneID = "w1:p2"
			fixture.fake.SnapshotValue.Panes = append(fixture.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle", Focused: true})
		}
	}
	if code, output := fixture.start(t); code != 0 || !strings.Contains(output, "state: working") {
		t.Fatalf("focused launch code=%d output=%s", code, output)
	}
	task, notices := fixture.onlyTask(t)
	if task.State != store.StateWorking || len(notices) != 0 || fixture.fake.CallCount("agent.prompt") != 1 {
		t.Fatalf("Task after focused typed launch = %#v notices=%#v prompts=%d", task, notices, fixture.fake.CallCount("agent.prompt"))
	}
}

func TestFailedAgentStartCanRelaunchOnSameSandbox(t *testing.T) {
	fixture := newStartFixture(t)
	fixture.fake.Errors["agent.start"] = stalled()
	if code, output := fixture.start(t); code != 1 {
		t.Fatalf("start: %d %s", code, output)
	}
	before, _ := fixture.onlyTask(t)
	fixture.fake.Errors["agent.start"] = nil
	if before.State != store.StateFailed || before.SandboxID == 0 {
		t.Fatalf("failed launch lost its Sandbox: %#v", before)
	}
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg, err := config.Load(fixture.home, fixture.project.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: "w1:p1", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"}}}
	project, err := db.ProjectByID(context.Background(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.relaunchTask(context.Background(), db, fixture.home, project, cfg, before, "")
	if err != nil {
		t.Fatalf("relaunch failed Task: %v", err)
	}
	after, err := db.TaskByID(context.Background(), fixture.project.ID, before.ID)
	if err != nil || after.State != store.StateWorking || after.SandboxID != before.SandboxID || after.WorktreePath != before.WorktreePath || result.Pane == "" {
		t.Fatalf("relaunch did not keep the Task Sandbox: before=%#v after=%#v result=%#v err=%v", before, after, result, err)
	}
}

func TestStartWaitsForNewSandboxShellBeforeStartingAgent(t *testing.T) {
	fixture := newStartFixture(t)
	fixture.fake.ErrorQueue["agent.start"] = []error{paneBusy(), paneBusy(), nil}
	if code, output := fixture.start(t); code != 0 || !strings.Contains(output, "state: working") {
		t.Fatalf("start into a starting shell code=%d output=%s", code, output)
	}
	if got := fixture.fake.CallCount("agent.start"); got != 3 {
		t.Fatalf("agent.start calls = %d, want 3", got)
	}
	if task, _ := fixture.onlyTask(t); task.State != store.StateWorking {
		t.Fatalf("Task after the shell became available = %#v", task)
	}
}

func TestStartFailsTaskWhenNewSandboxShellNeverBecomesAvailable(t *testing.T) {
	fixture := newStartFixture(t)
	fixture.fake.Errors["agent.start"] = paneBusy()
	if code, output := fixture.start(t); code != 1 || !strings.Contains(output, "agent_pane_busy") {
		t.Fatalf("start into a busy pane code=%d output=%s", code, output)
	}
	if got := fixture.fake.CallCount("agent.start"); got < 2 {
		t.Fatalf("agent.start calls = %d, want retries", got)
	}
	task, notices := fixture.onlyTask(t)
	if task.State != store.StateFailed || task.SandboxID == 0 {
		t.Fatalf("Task after a persistently busy pane lost its relaunchable Sandbox = %#v", task)
	}
	if len(notices) != 1 || notices[0].Kind != "task_failed" {
		t.Fatalf("Lead was not told about the busy pane: %#v", notices)
	}
}

func TestWaitAgentReadyReportsAgentBlockedAtDialog(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.Results["agent.get"] = json.RawMessage(`{"agent":{"agent_status":"blocked","interactive_ready":true,"launch_pending":false}}`)
	err := service.waitAgentReady(context.Background(), "w1:p1")
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "agent_blocked" {
		t.Fatalf("readiness error for an agent at a dialog = %v", err)
	}
}

func TestWaitAgentReadySurvivesTransientHerdrStall(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.ErrorQueue["agent.get"] = []error{&herdr.Error{Code: "herdr_unavailable", Message: "Herdr request failed"}, nil}
	if err := service.waitAgentReady(context.Background(), "w1:p1"); err != nil {
		t.Fatalf("waitAgentReady after one stalled poll: %v", err)
	}
}

func paneBusy() error {
	return &herdr.Error{Code: "agent_pane_busy", Message: "agent target pane w1:p1 is not an available shell"}
}

func TestStartAgentWaitsForShellStartupInNewPane(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.ErrorQueue["agent.start"] = []error{paneBusy(), paneBusy(), nil}
	if _, err := service.startAgent(context.Background(), map[string]any{"name": "w", "kind": "codex", "pane_id": "w1:p1"}); err != nil {
		t.Fatalf("startAgent: %v", err)
	}
	if got := fake.CallCount("agent.start"); got != 3 {
		t.Fatalf("agent.start calls = %d, want 3", got)
	}
}

func TestStartAgentReturnsRetryableErrorWhenPaneStaysBusy(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.Errors["agent.start"] = paneBusy()
	_, err := service.startAgent(context.Background(), map[string]any{"name": "w", "kind": "codex", "pane_id": "w1:p1"})
	var failure *axi.Error
	if !errors.As(err, &failure) || failure.Code != "agent_pane_busy" || !failure.Retryable {
		t.Fatalf("busy pane error = %v, want retryable agent_pane_busy", err)
	}
}

func TestStartAgentReturnsOtherErrorsWithoutRetry(t *testing.T) {
	service, fake := promptDeliveryService(t)
	fake.Errors["agent.start"] = &herdr.Error{Code: "agent_start_failed", Message: "codex exited"}
	if _, err := service.startAgent(context.Background(), map[string]any{"name": "w", "kind": "codex", "pane_id": "w1:p1"}); err == nil {
		t.Fatal("startAgent hid a start failure")
	}
	if got := fake.CallCount("agent.start"); got != 1 {
		t.Fatalf("agent.start calls = %d, want 1", got)
	}
}
