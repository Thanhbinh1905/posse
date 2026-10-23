package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/research"
	"github.com/thanhbinh1905/posse/internal/store"
)

// researchFixture is a registered Project with a Lead pane whose harness
// process ancestry and instance identities are faked.
type researchFixture struct {
	t          *testing.T
	root, repo string
	home       string
	db         *store.DB
	project    store.Project
	service    *Service
	identities map[int]string
	agentPID   int
}

const researchAgentPID = 20

func newResearchFixture(t *testing.T) *researchFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	repo, _ = filepath.EvalSymlinks(repo)
	home := filepath.Join(root, "posse")
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := &researchFixture{t: t, root: root, repo: repo, home: home, db: db, project: project, service: testService(home, herdr.NewFake()), identities: map[int]string{researchAgentPID: "boot/100"}, agentPID: researchAgentPID}
	previousRoot, previousIdentity := procRoot, researchProcessIdentity
	procRoot = researchFakeProc(t, repo, []string{"HERDR_PANE_ID=w1:p1"})
	researchProcessIdentity = func(pid int) (string, error) {
		if identity, ok := f.identities[pid]; ok {
			if identity == "" {
				return "", fmt.Errorf("no process %d", pid)
			}
			return identity, nil
		}
		return previousIdentity(pid)
	}
	t.Cleanup(func() { procRoot, researchProcessIdentity = previousRoot, previousIdentity })
	t.Chdir(repo)
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude-config"))
	return f
}

// researchFakeProc is fakeProc with the agent ancestor running claude.
func researchFakeProc(t *testing.T, agentCWD string, agentEnv []string) string {
	t.Helper()
	root := fakeProc(t, agentCWD, agentEnv)
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(researchAgentPID), "cmdline"), []byte("claude\x00--plugin-dir\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// hook runs one Claude hook event and returns its exit code and stderr.
func (f *researchFixture) hook(event map[string]any) (int, string, string) {
	f.t.Helper()
	if _, ok := event["session_id"]; !ok {
		event["session_id"] = "parent-session"
	}
	if _, ok := event["cwd"]; !ok {
		event["cwd"] = f.repo
	}
	data, err := json.Marshal(event)
	if err != nil {
		f.t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := f.service.RunResearchHook(bytes.NewReader(data), &stdout, &stderr, []string{"claude", "--version", "2.1.308"})
	return code, stdout.String(), stderr.String()
}

func (f *researchFixture) attempts() []store.ResearchAttempt {
	f.t.Helper()
	attempts, err := f.db.ResearchAttempts(context.Background(), store.ResearchAttemptFilter{ProjectID: f.project.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	return attempts
}

func agentCall(toolUseID, agentType string, extra map[string]any) map[string]any {
	input := map[string]any{"subagent_type": agentType, "description": "look", "prompt": "Find the store schema"}
	for key, value := range extra {
		input[key] = value
	}
	return map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_use_id": toolUseID, "tool_input": input}
}

func childCall(agentID, tool string, input map[string]any) map[string]any {
	return map[string]any{"hook_event_name": "PreToolUse", "agent_id": agentID, "agent_type": "Explore", "tool_name": tool, "tool_use_id": "child-" + tool, "tool_input": input}
}

func TestClaudeResearchHooksTrackReadOnlyChildren(t *testing.T) {
	f := newResearchFixture(t)
	ctx := context.Background()

	// Ordinary parent tool calls never touch the store.
	if code, _, stderr := f.hook(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Edit", "tool_input": map[string]any{}}); code != 0 || stderr != "" || len(f.attempts()) != 0 {
		t.Fatalf("parent Edit: code=%d stderr=%s", code, stderr)
	}

	// A write-capable general agent is refused visibly and recorded.
	code, _, stderr := f.hook(agentCall("tool-general", "general-purpose", nil))
	if code != guardExitBlocked || !strings.Contains(stderr, "refused") {
		t.Fatalf("general-purpose: code=%d stderr=%s", code, stderr)
	}
	attempts := f.attempts()
	if len(attempts) != 1 || attempts[0].State != "refused" || attempts[0].ChildID != "" {
		t.Fatalf("refused attempt %+v", attempts)
	}

	// Explore is registered with parent session, identity, tools and scope
	// before the harness starts it.
	if code, _, stderr := f.hook(agentCall("tool-explore", "Explore", nil)); code != 0 {
		t.Fatalf("Explore: code=%d stderr=%s", code, stderr)
	}
	registered := f.attempts()[0]
	if registered.State != "registered" || registered.ParentSession != "parent-session" || registered.ParentPID != researchAgentPID || registered.ParentIdentity != "boot/100" ||
		registered.Scope != f.repo || registered.AgentType != "Explore" || registered.Model != "inherit" || len(registered.Tools) == 0 || registered.HarnessVersion != "2.1.308" || registered.ParentRole != "lead" {
		t.Fatalf("registered %+v", registered)
	}
	// A repeated hook delivery does not duplicate the child.
	if code, _, _ := f.hook(agentCall("tool-explore", "Explore", nil)); code != 0 || len(f.attempts()) != 2 {
		t.Fatalf("duplicate registration: code=%d attempts=%d", code, len(f.attempts()))
	}
	// A second unbound Explore could not be bound unambiguously.
	if code, _, stderr := f.hook(agentCall("tool-explore-2", "Explore", nil)); code != guardExitBlocked || !strings.Contains(stderr, "has not started yet") {
		t.Fatalf("ambiguous second child: code=%d stderr=%s", code, stderr)
	}

	// A child without a registration of its type is refused tools.
	unknown := childCall("agent-unknown", "Read", map[string]any{"file_path": "go.mod"})
	unknown["agent_type"] = "general-purpose"
	if code, _, stderr := f.hook(unknown); code != guardExitBlocked || !strings.Contains(stderr, "not a registered") {
		t.Fatalf("untracked child: code=%d stderr=%s", code, stderr)
	}

	// SubagentStart binds the native child ID.
	code, stdout, _ := f.hook(map[string]any{"hook_event_name": "SubagentStart", "agent_id": "agent-1", "agent_type": "Explore"})
	if code != 0 || !strings.Contains(stdout, research.AttemptID(registered.ID)) {
		t.Fatalf("SubagentStart: code=%d stdout=%s", code, stdout)
	}
	bound, err := f.db.ResearchAttempt(ctx, registered.ID)
	if err != nil || bound.State != "running" || bound.ChildID != "agent-1" {
		t.Fatalf("bound %+v err=%v", bound, err)
	}

	// Inside the child, read-only tools pass and anything else is refused,
	// whatever permission mode the parent holds.
	for _, tc := range []struct {
		tool  string
		input map[string]any
		ok    bool
	}{
		{"Read", map[string]any{"file_path": "go.mod"}, true},
		{"Grep", map[string]any{"pattern": "x"}, true},
		{"Bash", map[string]any{"command": "git log --oneline -3"}, true},
		{"Bash", map[string]any{"command": "rm -rf internal"}, false},
		{"Edit", map[string]any{"file_path": "go.mod"}, false},
		{"Write", map[string]any{"file_path": "x"}, false},
		{"Agent", map[string]any{"subagent_type": "Explore"}, false},
	} {
		call := childCall("agent-1", tc.tool, tc.input)
		call["permission_mode"] = "bypassPermissions"
		code, _, stderr := f.hook(call)
		if (code == 0) != tc.ok {
			t.Errorf("%s %v: code=%d stderr=%s", tc.tool, tc.input, code, stderr)
		}
	}

	// SubagentStop accepts the bounded result and transcript usage.
	transcript := filepath.Join(f.root, "parent-session", "subagents", "agent-agent-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"id":"m1","model":"claude-haiku-5-5","usage":{"input_tokens":40,"output_tokens":9}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.hook(map[string]any{"hook_event_name": "SubagentStop", "agent_id": "agent-1", "agent_type": "Explore", "agent_transcript_path": transcript, "last_assistant_message": "The schema lives in internal/store/migrations."}); code != 0 {
		t.Fatalf("SubagentStop: code=%d stderr=%s", code, stderr)
	}
	completed, err := f.db.ResearchAttempt(ctx, registered.ID)
	if err != nil || completed.State != "completed" || completed.Model != "claude-haiku-5-5" || completed.SessionRef != transcript || completed.ResultBytes == 0 {
		t.Fatalf("completed %+v err=%v", completed, err)
	}
	if usage := research.ParseUsage(completed.Usage); usage.InputTokens != 40 || usage.OutputTokens != 9 {
		t.Fatalf("usage %s", completed.Usage)
	}
	data, err := os.ReadFile(completed.ResultPath)
	if err != nil || string(data) != "The schema lives in internal/store/migrations." || !strings.HasPrefix(completed.ResultPath, f.home) {
		t.Fatalf("result %s %q err=%v", completed.ResultPath, data, err)
	}
	// The completed child may no longer call tools.
	if code, _, _ := f.hook(childCall("agent-1", "Read", map[string]any{"file_path": "go.mod"})); code != guardExitBlocked {
		t.Fatalf("completed child kept tools: code=%d", code)
	}

	// The result is visible through the CLI with its pointer.
	code, output := runCLI(t, f.service, "research", "show", research.AttemptID(registered.ID), "--result")
	if code != 0 || !strings.Contains(output, "internal/store/migrations") || !strings.Contains(output, "completed") {
		t.Fatalf("show: %d %s", code, output)
	}
	code, output = runCLI(t, f.service, "research", "list", "--all")
	if code != 0 || !strings.Contains(output, "refused") || !strings.Contains(output, "agent-1") {
		t.Fatalf("list: %d %s", code, output)
	}
}

func TestClaudeResearchCancellationBindsToTheExactChild(t *testing.T) {
	f := newResearchFixture(t)
	ctx := context.Background()
	for i, agentType := range []string{"Explore", "Plan"} {
		if code, _, stderr := f.hook(agentCall(fmt.Sprintf("tool-%d", i), agentType, nil)); code != 0 {
			t.Fatalf("%s: %s", agentType, stderr)
		}
		if code, _, _ := f.hook(map[string]any{"hook_event_name": "SubagentStart", "agent_id": fmt.Sprintf("agent-%d", i), "agent_type": agentType}); code != 0 {
			t.Fatal("bind failed")
		}
	}
	explore, err := f.db.ResearchAttemptByChild(ctx, "claude", "agent-0")
	if err != nil {
		t.Fatal(err)
	}
	code, output := runCLI(t, f.service, "research", "cancel", research.AttemptID(explore.ID))
	if code != 0 || !strings.Contains(output, "cancelled") {
		t.Fatalf("cancel: %d %s", code, output)
	}
	if code, _, stderr := f.hook(childCall("agent-0", "Read", map[string]any{"file_path": "go.mod"})); code != guardExitBlocked || !strings.Contains(stderr, "cancelled") {
		t.Fatalf("cancelled child: code=%d stderr=%s", code, stderr)
	}
	planChild := childCall("agent-1", "Read", map[string]any{"file_path": "go.mod"})
	planChild["agent_type"] = "Plan"
	if code, _, stderr := f.hook(planChild); code != 0 {
		t.Fatalf("sibling child was affected: %s", stderr)
	}
	// Output after cancellation is not accepted.
	if code, _, _ := f.hook(map[string]any{"hook_event_name": "SubagentStop", "agent_id": "agent-0", "agent_type": "Explore", "last_assistant_message": "late"}); code != 0 {
		t.Fatal("stop failed")
	}
	cancelled, err := f.db.ResearchAttempt(ctx, explore.ID)
	if err != nil || cancelled.State != "cancelled" || cancelled.ResultPath != "" || !strings.Contains(cancelled.Note, "result discarded") {
		t.Fatalf("cancelled %+v err=%v", cancelled, err)
	}
	if code, output := runCLI(t, f.service, "research", "cancel", research.AttemptID(explore.ID)); code == 0 || !strings.Contains(output, "research_not_active") {
		t.Fatalf("second cancel: %d %s", code, output)
	}
}

func TestClaudeResearchParentRestartInterruptsOrResumes(t *testing.T) {
	f := newResearchFixture(t)
	ctx := context.Background()
	for i, agentType := range []string{"Explore", "Plan"} {
		if code, _, stderr := f.hook(agentCall(fmt.Sprintf("tool-%d", i), agentType, nil)); code != 0 {
			t.Fatal(stderr)
		}
	}
	transcriptDir := filepath.Join(f.root, "sessions", "parent-session", "subagents")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcriptDir, "agent-agent-0.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := map[string]any{"hook_event_name": "SubagentStart", "agent_id": "agent-0", "agent_type": "Explore", "transcript_path": filepath.Join(f.root, "sessions", "parent-session.jsonl")}
	if code, _, _ := f.hook(start); code != 0 {
		t.Fatal("bind failed")
	}
	// The parent Claude process instance exits; Plan never started.
	f.identities[researchAgentPID] = ""
	code, output := runCLI(t, f.service, "research", "list", "--all")
	if code != 0 {
		t.Fatal(output)
	}
	explore, _ := f.db.ResearchAttemptByChild(ctx, "claude", "agent-0")
	if explore.State != "interrupted" || !explore.Resumable {
		t.Fatalf("explore %+v", explore)
	}
	var plan store.ResearchAttempt
	for _, attempt := range f.attempts() {
		if attempt.AgentType == "Plan" {
			plan = attempt
		}
	}
	if plan.State != "interrupted" || plan.Resumable {
		t.Fatalf("plan %+v", plan)
	}
	// A different session cannot continue the child; the resumed parent can.
	other := childCall("agent-0", "Read", map[string]any{"file_path": "go.mod"})
	other["session_id"] = "other-session"
	if code, _, _ := f.hook(other); code != guardExitBlocked {
		t.Fatal("foreign session resumed the child")
	}
	f.identities[researchAgentPID] = "boot/200"
	if code, _, stderr := f.hook(childCall("agent-0", "Read", map[string]any{"file_path": "go.mod"})); code != 0 {
		t.Fatalf("resume: %s", stderr)
	}
	resumed, _ := f.db.ResearchAttempt(ctx, explore.ID)
	if resumed.State != "running" || resumed.ParentIdentity != "boot/200" {
		t.Fatalf("resumed %+v", resumed)
	}
}

func TestClaudeResearchRefusesBudgetVersionAndForeignScope(t *testing.T) {
	f := newResearchFixture(t)
	if code, output := runCLI(t, f.service, "config", "set", "research.max_active", "1"); code != 0 {
		t.Fatal(output)
	}
	if code, _, stderr := f.hook(agentCall("tool-a", "Explore", nil)); code != 0 {
		t.Fatal(stderr)
	}
	if code, _, stderr := f.hook(agentCall("tool-b", "Plan", nil)); code != guardExitBlocked || !strings.Contains(stderr, "budget") {
		t.Fatalf("budget: code=%d stderr=%s", code, stderr)
	}
	outside := t.TempDir()
	call := agentCall("tool-c", "Plan", nil)
	call["cwd"] = outside
	if code, _, stderr := f.hook(call); code != guardExitBlocked || !strings.Contains(stderr, "outside") {
		t.Fatalf("scope: code=%d stderr=%s", code, stderr)
	}
	data, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": f.repo, "hook_event_name": "PreToolUse", "tool_name": "Agent", "tool_use_id": "tool-d", "tool_input": map[string]any{"subagent_type": "Plan"}})
	var stdout, stderr bytes.Buffer
	if code := f.service.RunResearchHook(bytes.NewReader(data), &stdout, &stderr, []string{"claude", "--version", "2.0.1"}); code != guardExitBlocked || !strings.Contains(stderr.String(), "older") {
		t.Fatalf("old Claude: code=%d stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	if code := f.service.RunResearchHook(bytes.NewReader(data), &stdout, &stderr, []string{"claude"}); code != guardExitBlocked || !strings.Contains(stderr.String(), "version") {
		t.Fatalf("unknown Claude version: code=%d stderr=%s", code, stderr.String())
	}
	refused := 0
	for _, attempt := range f.attempts() {
		if attempt.State == "refused" {
			refused++
		}
	}
	if refused != 4 {
		t.Fatalf("refused attempts=%d %+v", refused, f.attempts())
	}
}

func TestClaudeResearchPluginInstallsHooksOnlyWhenTracked(t *testing.T) {
	f := newResearchFixture(t)
	probes := 0
	version := "2.1.308 (Claude Code)"
	previous := researchHarnessVersion
	researchHarnessVersion = func(context.Context, research.Harness) string { probes++; return version }
	t.Cleanup(func() { researchHarnessVersion = previous })
	cfg, err := f.service.researchConfig(f.home, f.project)
	if err != nil {
		t.Fatal(err)
	}
	args, err := appendResearchLaunchArgs(context.Background(), f.home, f.project, cfg, "claude", []string{"--model", "x"})
	if err != nil || len(args) != 4 || args[2] != "--plugin-dir" {
		t.Fatalf("args=%v err=%v", args, err)
	}
	// Launching never runs the harness binary.
	if probes != 0 {
		t.Fatalf("launch probed the Claude binary %d times", probes)
	}
	hooks, err := os.ReadFile(filepath.Join(args[3], "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PreToolUse", "SubagentStart", "SubagentStop", "_research-hook claude"} {
		if !strings.Contains(string(hooks), want) {
			t.Fatalf("hooks.json missing %q: %s", want, hooks)
		}
	}
	if strings.Contains(string(hooks), "--version") {
		t.Fatalf("hooks.json pins a version: %s", hooks)
	}
	if args, _ := appendResearchLaunchArgs(context.Background(), f.home, f.project, cfg, "codex", []string{"x"}); len(args) != 1 {
		t.Fatalf("codex args=%v", args)
	}
	cfg.Research.Native = "off"
	if args, _ := appendResearchLaunchArgs(context.Background(), f.home, f.project, cfg, "claude", []string{"x"}); len(args) != 1 {
		t.Fatalf("off args=%v", args)
	}

	// The hook probes the version only when an Agent call needs admission.
	hook := func(event map[string]any) (int, string) {
		event["session_id"], event["cwd"] = "parent-session", f.repo
		data, _ := json.Marshal(event)
		var stdout, stderr bytes.Buffer
		code := f.service.RunResearchHook(bytes.NewReader(data), &stdout, &stderr, []string{"claude"})
		return code, stderr.String()
	}
	if code, _ := hook(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_input": map[string]any{}}); code != 0 || probes != 0 {
		t.Fatalf("ordinary tool call: code=%d probes=%d", code, probes)
	}
	if code, stderr := hook(agentCall("tool-1", "Explore", nil)); code != 0 || probes != 1 {
		t.Fatalf("admission: code=%d probes=%d stderr=%s", code, probes, stderr)
	}
	version = ""
	if code, stderr := hook(agentCall("tool-2", "Plan", nil)); code != guardExitBlocked || !strings.Contains(stderr, "version") {
		t.Fatalf("unknown version: code=%d stderr=%s", code, stderr)
	}
}
