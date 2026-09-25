package app

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestFreshSetupProfilesResolveShipDispatch(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		kinds     []string
		leadKind  string
		wantKinds []string
	}{
		{name: "claude preferred", kinds: []string{"claude", "codex"}, leadKind: "claude", wantKinds: []string{"claude", "codex"}},
		{name: "codex fallback", kinds: []string{"codex"}, leadKind: "codex", wantKinds: []string{"codex"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			setupFollowupAgentPath(t, testCase.kinds...)
			repo := filepath.Join(root, "repo")
			initRepo(t, repo)
			home := filepath.Join(root, "posse-home")
			userHome := filepath.Join(root, "user-home")
			t.Setenv("HOME", userHome)
			t.Setenv("POSSE_HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(userHome, ".claude"))
			t.Setenv("CODEX_HOME", filepath.Join(userHome, ".codex"))

			adapter := herdr.NewFake()
			adapter.RunOut["plugin list --json"] = []byte(`{"id":"cli:plugin","result":{"type":"plugin_list","plugins":[]}}`)
			adapter.RunOut["plugin link "+filepath.Join(home, "plugin", "herdr-plugin.toml")] = []byte("{}")
			adapter.RunOut["integration status"] = []byte("claude: not installed\ncodex: not installed\n")
			adapter.RunOut["integration install claude"] = []byte("installed")
			adapter.RunOut["integration install codex"] = []byte("installed")
			service := testService(home, adapter)
			setupOutput := &bytes.Buffer{}
			cli := service.CLI()
			cli.Out, cli.ErrOut = setupOutput, setupOutput
			if code := cli.Run([]string{"setup", "--binary", "/usr/bin/true"}); code != 0 {
				t.Fatalf("fresh setup exit=%d output=%s", code, setupOutput)
			}

			cfg, err := config.Load(home, "")
			if err != nil {
				t.Fatalf("load generated config: %v", err)
			}
			if cfg.Lead.Kind != testCase.leadKind || cfg.DispatchDefault.Use != testCase.leadKind {
				t.Fatalf("generated Lead/default Profile = %q/%q, want %q", cfg.Lead.Kind, cfg.DispatchDefault.Use, testCase.leadKind)
			}
			if len(cfg.Profiles) != len(testCase.wantKinds) {
				t.Fatalf("generated Profiles = %#v, want kinds %v", cfg.Profiles, testCase.wantKinds)
			}
			for _, kind := range testCase.wantKinds {
				profile, ok := cfg.Profiles[kind]
				if !ok || profile.Kind != kind || profile.Model != "" || profile.Effort != "" {
					t.Fatalf("generated Profile %q = %#v, found=%v", kind, profile, ok)
				}
			}
			if cfg.Identity.Lead.Name != "Sheriff" || cfg.Identity.Worker.DisplayPrefix != "rider" {
				t.Fatalf("generated Identity defaults = %#v", cfg.Identity)
			}

			cli = service.CLI()
			projectOutput := &bytes.Buffer{}
			cli.Out, cli.ErrOut = projectOutput, projectOutput
			t.Chdir(repo)
			if code := cli.Run([]string{"project", "add", "--name", "fresh"}); code != 0 {
				t.Fatalf("register Project exit=%d output=%s", code, projectOutput)
			}
			briefPath := filepath.Join(root, "ship-brief.md")
			brief := "---\ntype: ship\ntitle: First run\ndone_when: dispatch resolves the generated Profile\n---\nRun the first Worker.\n"
			if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
				t.Fatal(err)
			}
			dispatchOutput, dispatchErrors := &bytes.Buffer{}, &bytes.Buffer{}
			cli = service.CLI()
			cli.Out, cli.ErrOut = dispatchOutput, dispatchErrors
			if code := cli.Run([]string{"dispatch", "--brief", briefPath, "--json"}); code != 0 {
				t.Fatalf("dispatch exit=%d output=%s stderr=%s", code, dispatchOutput, dispatchErrors)
			}
			var result struct {
				Profile      string `json:"profile"`
				DispatchRule string `json:"dispatch_rule"`
			}
			if err := json.Unmarshal(dispatchOutput.Bytes(), &result); err != nil {
				t.Fatalf("dispatch output is not JSON: %s: %v", dispatchOutput, err)
			}
			if result.Profile != testCase.leadKind || result.DispatchRule != "dispatch.default" {
				t.Fatalf("fresh ship dispatch = %#v, want default Profile %q", result, testCase.leadKind)
			}
		})
	}
}

func TestRideAndDispatchExplainHowToConfigureMissingDefault(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	home := filepath.Join(root, "posse-home")
	service := testService(home, nil)
	t.Chdir(repo)
	projectOutput := &bytes.Buffer{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = projectOutput, projectOutput
	if code := cli.Run([]string{"project", "add", "--name", "missing-default"}); code != 0 {
		t.Fatalf("register Project exit=%d output=%s", code, projectOutput)
	}
	briefPath := filepath.Join(root, "ship-brief.md")
	brief := "---\ntype: ship\ntitle: Missing default\ndone_when: report setup guidance\n---\nThis should not start without a Profile.\n"
	if err := os.WriteFile(briefPath, []byte(brief), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{"dispatch", "ride"} {
		t.Run(command, func(t *testing.T) {
			output, errorsOut := &bytes.Buffer{}, &bytes.Buffer{}
			cli := service.CLI()
			cli.Out, cli.ErrOut = output, errorsOut
			args := []string{command, "--brief", briefPath, "--json"}
			if command == "ride" {
				args = []string{command, "--brief", briefPath, "--name", "missing-default", "--json"}
			}
			if code := cli.Run(args); code != 1 {
				t.Fatalf("%s exit=%d output=%s stderr=%s", command, code, output, errorsOut)
			}
			var failure axi.Error
			if err := json.Unmarshal(output.Bytes(), &failure); err != nil {
				t.Fatalf("%s did not return a structured error: output=%s err=%v", command, output, err)
			}
			if failure.Code != "profile_required" {
				t.Fatalf("%s error code=%q, want profile_required: %#v", command, failure.Code, failure)
			}
			help := strings.Join(failure.Help, "\n")
			for _, want := range []string{"posse config set dispatch.default.use <profile>", "/posse-setup"} {
				if !strings.Contains(help, want) {
					t.Errorf("%s help omitted %q: %#v", command, want, failure.Help)
				}
			}
		})
	}
}

func setupFollowupAgentPath(t *testing.T, kinds ...string) string {
	t.Helper()
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git for isolated test PATH: %v", err)
	}
	if err := os.Symlink(gitPath, filepath.Join(binDir, "git")); err != nil {
		t.Fatal(err)
	}
	for _, name := range append([]string{"herdr"}, kinds...) {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	return binDir
}
