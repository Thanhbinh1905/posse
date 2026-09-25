package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func humanSetupFixture(t *testing.T) (*Service, *herdr.Fake, string) {
	t.Helper()
	root := t.TempDir()
	setupFollowupAgentPath(t, "claude")
	home := filepath.Join(root, "posse-home")
	userHome := filepath.Join(root, "user-home")
	t.Setenv("HOME", userHome)
	t.Setenv("POSSE_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(userHome, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(userHome, ".codex"))
	adapter := herdr.NewFake()
	adapter.Server.Running = false
	adapter.RunOut["integration status"] = []byte("claude: not installed\ncodex: not installed\n")
	adapter.RunOut["integration install claude"] = []byte("installed")
	adapter.RunOut["plugin list --json"] = []byte(`{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[]}}`)
	adapter.RunOut["plugin link "+filepath.Join(home, "plugin", "herdr-plugin.toml")] = []byte("{}")
	return testService(home, adapter), adapter, userHome
}

func runHumanSetup(t *testing.T, service *Service, args ...string) (string, int) {
	t.Helper()
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	code := cli.Run(append([]string{"setup", "--human", "--binary", "/usr/bin/true"}, args...))
	return output.String(), code
}

func assertNoTOON(t *testing.T, output string) {
	t.Helper()
	for _, marker := range []string{"error{", "plan[", "{step,", "prerequisites[", "help["} {
		if strings.Contains(output, marker) {
			t.Fatalf("human setup output contains TOON marker %q:\n%s", marker, output)
		}
	}
}

func TestHumanSetupAppliesWithoutHerdrServer(t *testing.T) {
	service, adapter, userHome := humanSetupFixture(t)

	preview, code := runHumanSetup(t, service, "--check")
	if code != 0 {
		t.Fatalf("human preview exit=%d output=%s", code, preview)
	}
	if pending, code := runHumanSetup(t, service, "--check", "--exit-code"); code != setupPendingExitCode || pending != preview {
		t.Fatalf("pending setup --check --exit-code exit=%d, want %d with the same checklist:\n%s", code, setupPendingExitCode, pending)
	}
	assertNoTOON(t, preview)
	for _, want := range []string{
		"+  Install the Herdr integration for claude\n",
		"+  Link the posse plugin into Herdr\n",
		"+  Install the posse-setup skill\n",
		"+  Add the Claude Code SessionStart hook to ~/.claude/settings.json\n",
		"+  Add the Codex SessionStart hook to ~/.codex/hooks.json; Codex asks once to trust it\n",
		"✓  claude is on PATH\n",
	} {
		if !strings.Contains(preview, want) {
			t.Errorf("human preview omitted %q:\n%s", want, preview)
		}
	}
	if _, err := os.Stat(filepath.Join(userHome, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("human preview changed the machine: %v", err)
	}

	applied, code := runHumanSetup(t, service)
	if code != 0 {
		t.Fatalf("human setup exit=%d output=%s", code, applied)
	}
	assertNoTOON(t, applied)
	for _, want := range []string{
		"✓  Installed the Herdr integration for claude\n",
		"✓  Linked the posse plugin into Herdr\n",
		"✓  Installed the posse skill\n",
		"✓  Added the Claude Code SessionStart hook to ~/.claude/settings.json\n",
	} {
		if !strings.Contains(applied, want) {
			t.Errorf("human setup omitted %q:\n%s", want, applied)
		}
	}
	if len(adapter.Calls) != 0 {
		t.Fatalf("setup used the Herdr socket although the server is not running: %#v", adapter.Calls)
	}

	adapter.RunOut["integration status"] = []byte("claude: current (v9)\ncodex: not installed\n")
	adapter.RunOut["plugin list --json"] = []byte(`{"result":{"plugins":[{"plugin_id":"posse.herdr","manifest_path":"` + filepath.Join(service.Home, "plugin", "herdr-plugin.toml") + `"}]}}`)
	again, code := runHumanSetup(t, service, "--check", "--exit-code")
	if code != 0 {
		t.Fatalf("complete setup --check --exit-code exit=%d output=%s", code, again)
	}
	if strings.Contains(again, "+  ") {
		t.Fatalf("completed setup still lists pending changes:\n%s", again)
	}
	if !strings.Contains(again, "-  posse plugin already linked into Herdr\n") || !strings.Contains(again, "-  Codex SessionStart hook already present\n") {
		t.Fatalf("completed setup did not report kept items:\n%s", again)
	}
}

func TestHumanSetupPreviewFailsReadablyOnConflict(t *testing.T) {
	service, _, userHome := humanSetupFixture(t)
	skill := filepath.Join(userHome, ".agents", "skills", "posse", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("my own skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, code := runHumanSetup(t, service, "--check")
	if code == 0 {
		t.Fatalf("human preview accepted an unmanaged skill path:\n%s", output)
	}
	assertNoTOON(t, output)
	if !strings.Contains(output, "✗  setup would overwrite an unmanaged path\n") || !strings.Contains(output, "rerun `posse setup`") {
		t.Fatalf("human conflict output is not readable:\n%s", output)
	}
}

func TestSetupCheckExitCodeKeepsAgentOutput(t *testing.T) {
	service, _, _ := humanSetupFixture(t)
	output := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"setup", "--check", "--exit-code", "--binary", "/usr/bin/true"}); code != setupPendingExitCode {
		t.Fatalf("setup --check --exit-code exit=%d, want %d: %s", code, setupPendingExitCode, output)
	}
	if !strings.Contains(output.String(), "{step,target,action,note}:") || strings.Contains(output.String(), "error{") {
		t.Fatalf("--exit-code changed the TOON plan output:\n%s", output)
	}
	output.Reset()
	cli = service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"setup", "--check", "--binary", "/usr/bin/true"}); code != 0 {
		t.Fatalf("setup --check without --exit-code exit=%d: %s", code, output)
	}
	output.Reset()
	cli = service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"setup", "--exit-code", "--binary", "/usr/bin/true"}); code != 2 {
		t.Fatalf("setup --exit-code without --check exit=%d: %s", code, output)
	}
}

func TestHumanSetupRejectsUninstallAndJSON(t *testing.T) {
	service, _, _ := humanSetupFixture(t)
	for _, extra := range []string{"--uninstall", "--json"} {
		output, code := runHumanSetup(t, service, extra)
		if code != 2 {
			t.Fatalf("setup --human %s exit=%d output=%s", extra, code, output)
		}
	}
}
