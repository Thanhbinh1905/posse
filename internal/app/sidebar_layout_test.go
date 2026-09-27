package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSidebarLayoutPreservesHerdrConfigAndIsIdempotent(t *testing.T) {
	for _, original := range []string{
		"# favorite theme\nonboarding = false\n\n[theme]\nname = 'catppuccin' # keep this comment\n",
		"[ui.sidebar.agents] # existing table\nrow_gap = 1 # keep gap\n\n[ui.sidebar.spaces]\nrow_gap = 2\n",
		"[ui.sidebar.agents.rows_by_agent]\nclaude = [[\"agent\"]] # keep override\n",
	} {
		t.Run(strings.Split(original, "\n")[0], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "herdr", "config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
				t.Fatal(err)
			}
			if state, err := sidebarLayoutState(path); state != "offer" || err != nil {
				t.Fatalf("state = %q, %v", state, err)
			}
			changed, err := installSidebarLayout(path)
			if err != nil || !changed {
				t.Fatalf("install = %t, %v", changed, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSuffix(original, "\n"), "\n") {
				if !strings.Contains(string(data), line) {
					t.Errorf("lost config line %q in %s", line, data)
				}
			}
			if state, err := sidebarLayoutState(path); state != "keep" || err != nil {
				t.Fatalf("after install = %q, %v", state, err)
			}
			changed, err = installSidebarLayout(path)
			if err != nil || changed {
				t.Fatalf("repeat install = %t, %v", changed, err)
			}
			again, _ := os.ReadFile(path)
			if string(data) != string(again) {
				t.Fatal("idempotent install changed the bytes")
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0o640 {
				t.Fatalf("file mode = %v", info.Mode())
			}
		})
	}
}

func TestHerdrVersionGatesStyledSidebarRules(t *testing.T) {
	for _, tc := range []struct {
		version   string
		supported bool
	}{
		{"herdr 0.9.0", false}, {"herdr 0.9.1", true}, {"herdr 0.9.10", true}, {"herdr 0.10.0", true}, {"herdr dev", false},
	} {
		if got := herdrSupportsSidebarRules(tc.version); got != tc.supported {
			t.Errorf("%s: supported=%t, want %t", tc.version, got, tc.supported)
		}
	}
}

func TestSidebarLayoutDoesNotReplaceCustomRows(t *testing.T) {
	original := "# custom\n[ui.sidebar.agents]\nrows = [[\"tab\"]] # preferred\n"
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := sidebarLayoutState(path); state != "manual" || err != nil {
		t.Fatalf("state = %q, %v", state, err)
	}
	changed, err := installSidebarLayout(path)
	if err != nil || changed {
		t.Fatalf("install = %t, %v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Fatalf("custom layout replaced: %s", data)
	}
}

func TestDoctorReportsInstalledHerdrSidebarLayout(t *testing.T) {
	service, _, userHome := humanSetupFixture(t)
	configHome := filepath.Join(userHome, "xdg")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(configHome, "herdr", "config.toml")
	if changed, err := installSidebarLayout(path); !changed || err != nil {
		t.Fatalf("install = %t %v", changed, err)
	}
	output := &strings.Builder{}
	cli := service.CLI()
	cli.Out, cli.ErrOut = output, output
	if code := cli.Run([]string{"doctor"}); code != 0 {
		t.Fatalf("doctor exit=%d: %s", code, output)
	}
	if !strings.Contains(output.String(), "Herdr Agents sidebar layout,ok,Posse layout present") {
		t.Errorf("doctor did not recognize installed layout: %s", output)
	}
}

func TestSetupPreviewLeavesExistingAgentsRowsForManualReview(t *testing.T) {
	service, _, userHome := humanSetupFixture(t)
	configHome := filepath.Join(userHome, "xdg")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(configHome, "herdr", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("# My own rows\n[ui.sidebar.agents]\nrows = [[\"tab\"]]\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	preview, code := runHumanSetup(t, service, "--check")
	if code != 0 || !strings.Contains(preview, "rows already configured") || !strings.Contains(preview, "$posse_row") {
		t.Fatalf("manual preview = %d %s", code, preview)
	}
	applied, code := runHumanSetup(t, service, "--sidebar-layout")
	if code != 0 || !strings.Contains(applied, "rows already configured") {
		t.Fatalf("setup with custom rows = %d %s", code, applied)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(original) {
		t.Fatalf("custom rows changed: %s %v", data, err)
	}
}

func TestSetupReportsSidebarLayoutAndAppliesOnlyWhenConfirmed(t *testing.T) {
	service, adapter, userHome := humanSetupFixture(t)
	configHome := filepath.Join(userHome, "xdg")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(configHome, "herdr", "config.toml")
	preview, code := runHumanSetup(t, service, "--check")
	if code != 0 || !strings.Contains(preview, "Add Posse Agents sidebar layout") {
		t.Fatalf("preview = %d %s", code, preview)
	}
	applied, code := runHumanSetup(t, service, "--no-sidebar-layout")
	if code != 0 || !strings.Contains(applied, "Add Posse Agents sidebar layout") {
		t.Fatalf("ordinary setup = %d %s", code, applied)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("layout applied without confirmation: %v", err)
	}
	adapter.RunOut["integration status"] = []byte("claude: current (v9)\ncodex: not installed\n")
	adapter.RunOut["plugin list --json"] = []byte(`{"result":{"plugins":[{"plugin_id":"posse.herdr","manifest_path":"` + filepath.Join(service.Home, "plugin", "herdr-plugin.toml") + `"}]}}`)
	preview, code = runHumanSetup(t, service, "--check", "--exit-code")
	if code != 0 || !strings.Contains(preview, "Add Posse Agents sidebar layout") {
		t.Fatalf("optional offer made required: %d %s", code, preview)
	}
	applied, code = runHumanSetup(t, service, "--sidebar-layout")
	if code != 0 || !strings.Contains(applied, "Added Posse Agents sidebar layout") {
		t.Fatalf("confirmed setup = %d %s", code, applied)
	}
	if state, err := sidebarLayoutState(path); state != "keep" || err != nil {
		t.Fatalf("confirmed state = %q, %v", state, err)
	}
	preview, code = runHumanSetup(t, service, "--check", "--exit-code")
	if code != 0 || !strings.Contains(preview, "sidebar layout already present") {
		t.Fatalf("complete = %d %s", code, preview)
	}
}
