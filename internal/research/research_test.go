package research

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAgent(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAdmitClaudeAgentRefusesWriteCapableChildren(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "repo")
	configDir := filepath.Join(root, "claude")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if agent, err := AdmitClaudeAgent(ClaudeLaunch{SubagentType: "Explore", Prompt: "map the store"}, project, configDir); err != nil || agent.Source != "builtin" || agent.Model != "inherit" {
		t.Fatalf("builtin Explore: %+v %v", agent, err)
	}
	for _, launch := range []ClaudeLaunch{
		{SubagentType: "general-purpose"},
		{},
		{SubagentType: "Explore", Isolation: "worktree"},
		{SubagentType: "Explore", Mode: "bypassPermissions"},
		{SubagentType: "Explore", Name: "helper", TeamName: "swarm"},
		{SubagentType: "plugin:reader"},
	} {
		if _, err := AdmitClaudeAgent(launch, project, configDir); err == nil {
			t.Errorf("admitted %+v", launch)
		}
	}

	// A project definition that overrides Explore with write tools is refused,
	// even though the name matches a read-only builtin.
	writeAgent(t, filepath.Join(project, ".claude", "agents"), "explore", "---\nname: Explore\ntools: Read, Edit, Bash\n---\nbody")
	if _, err := AdmitClaudeAgent(ClaudeLaunch{SubagentType: "Explore"}, project, configDir); err == nil || !strings.Contains(err.Error(), "Edit") {
		t.Fatalf("overridden Explore err=%v", err)
	}
	writeAgent(t, filepath.Join(project, ".claude", "agents"), "explore", "---\nname: Explore\n---\ninherits every tool")
	if _, err := AdmitClaudeAgent(ClaudeLaunch{SubagentType: "Explore"}, project, configDir); err == nil || !strings.Contains(err.Error(), "inherits") {
		t.Fatalf("tool-inheriting Explore err=%v", err)
	}
	writeAgent(t, filepath.Join(configDir, "agents"), "reader", "---\nname: reader\ntools:\n  - Read\n  - Grep\nmodel: haiku\npermissionMode: bypassPermissions\n---\n")
	if _, err := AdmitClaudeAgent(ClaudeLaunch{SubagentType: "reader"}, project, configDir); err == nil || !strings.Contains(err.Error(), "bypassPermissions") {
		t.Fatalf("bypass definition err=%v", err)
	}
	writeAgent(t, filepath.Join(configDir, "agents"), "reader", "---\nname: reader\ntools:\n  - Read\n  - Grep\n  - Bash\nmodel: haiku\n---\n")
	agent, err := AdmitClaudeAgent(ClaudeLaunch{SubagentType: "reader"}, project, configDir)
	if err != nil || agent.Model != "haiku" || strings.Join(agent.Tools, ",") != "Read,Grep,Bash" {
		t.Fatalf("read-only custom agent: %+v %v", agent, err)
	}
	writeAgent(t, filepath.Join(configDir, "agents"), "reader", "---\nname: reader\ntools: [Read]\nmcpServers:\n  github: {}\n---\n")
	if _, err := AdmitClaudeAgent(ClaudeLaunch{SubagentType: "reader"}, project, configDir); err == nil {
		t.Fatal("admitted an agent with MCP servers")
	}
}

func TestReadOnlyCommand(t *testing.T) {
	for _, command := range []string{
		"git log --oneline -5",
		"git -C internal --no-pager diff HEAD~1 -- store",
		"rg -n 'research' internal | head -20",
		"find . -name '*.go' -type f | wc -l",
		"cat go.mod 2>/dev/null && ls -la $HOME",
		"grep -rn foo . 2>&1 | sort",
	} {
		if err := ReadOnlyCommand(command); err != nil {
			t.Errorf("%q refused: %v", command, err)
		}
	}
	for _, command := range []string{
		"rm -rf internal",
		"echo hi > notes.md",
		"cat a >> b",
		"find . -delete",
		"find . -exec rm {} ;",
		"git commit -am x",
		"git -c core.pager=sh log",
		"git diff --output=patch",
		"sed -i s/a/b/ file",
		"cat $(touch x)",
		"(cd /tmp && ls)",
		"ls &",
		"FOO=1 ls",
		"sort -o out in",
		"rg --pre ./run x",
		"bash -c 'ls'",
		"ls >& out",
	} {
		if err := ReadOnlyCommand(command); err == nil {
			t.Errorf("%q admitted", command)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	if version, err := CheckVersion(Claude, "2.1.308 (Claude Code)"); err != nil || version != "2.1.308" {
		t.Fatalf("claude: %s %v", version, err)
	}
	if _, err := CheckVersion(Codex, "codex-cli 0.163.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckVersion(Codex, "codex-cli 0.99.9"); err == nil {
		t.Fatal("old codex admitted")
	}
	if _, err := CheckVersion(Pi, ""); err == nil {
		t.Fatal("unknown pi version admitted")
	}
	if _, err := CheckVersion(Pi, "1.10.0"); err != nil {
		t.Fatal(err)
	}
}

func TestEventParsers(t *testing.T) {
	codex := NewEventParser(Codex)
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"first"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"ls"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"final answer"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":20,"output_tokens":30}}`,
		`not json`,
	} {
		codex.Consume([]byte(line))
	}
	progress := codex.Progress()
	if progress.ChildID != "0199a213-81c0-7800-8aa1-bbab2a035a53" || progress.Result != "final answer" || progress.Usage.InputTokens != 120 || progress.Usage.CacheReadTokens != 20 || progress.Usage.Turns != 1 {
		t.Fatalf("codex progress %+v", progress)
	}
	pi := NewEventParser(Pi)
	for _, line := range []string{
		`{"type":"session","id":"pi-session-1","cwd":"/repo"}`,
		`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"prompt"}]}}`,
		`{"type":"message_end","message":{"role":"assistant","model":"m1","content":[{"type":"toolCall","name":"read"}],"usage":{"input":10,"output":2,"cacheRead":1,"cacheWrite":0,"cost":{"total":0.001}}}}`,
		`{"type":"message_end","message":{"role":"assistant","model":"m1","content":[{"type":"text","text":"done"}],"usage":{"input":20,"output":5,"cacheRead":0,"cacheWrite":3,"cost":{"total":0.002}}}}`,
	} {
		pi.Consume([]byte(line))
	}
	progress = pi.Progress()
	if progress.ChildID != "pi-session-1" || progress.Result != "done" || progress.Model != "m1" || progress.Usage.InputTokens != 30 || progress.Usage.Turns != 2 || progress.Usage.CacheWriteTokens != 3 {
		t.Fatalf("pi progress %+v", progress)
	}
	pi.Consume([]byte(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"rate limited"}}`))
	if pi.Progress().Error != "rate limited" {
		t.Fatalf("pi error %+v", pi.Progress())
	}
}

func TestClaudeUsageCountsEachMessageOnce(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":50}}}`,
		`{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku","usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":50}}}`,
		`{"type":"assistant","message":{"id":"msg_2","model":"claude-haiku","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":7}}}`,
	}, "\n")
	usage, model, err := claudeUsage(strings.NewReader(transcript))
	if err != nil || model != "claude-haiku" || usage.InputTokens != 110 || usage.OutputTokens != 45 || usage.CacheReadTokens != 50 || usage.CacheWriteTokens != 7 || usage.Turns != 2 {
		t.Fatalf("usage=%+v model=%s err=%v", usage, model, err)
	}
}
