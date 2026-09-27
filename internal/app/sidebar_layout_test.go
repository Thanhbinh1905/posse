package app

import (
	"encoding/json"
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

func TestSidebarLayoutReadsThroughSymlinksWithoutWritingThem(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "no rows", content: "[theme]\nname = 'catppuccin'\n", want: "symlink"},
		{name: "Posse rows", content: sidebarLayoutSnippet, want: "keep"},
		{name: "custom rows", content: "[ui.sidebar.agents]\nrows = [[\"tab\"]]\n", want: "manual"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "managed-config.toml")
			path := filepath.Join(root, "xdg", "herdr", "config.toml")
			if err := os.WriteFile(source, []byte(testCase.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(source, path); err != nil {
				t.Fatal(err)
			}
			if state, err := sidebarLayoutState(path); state != testCase.want || err != nil {
				t.Fatalf("state = %q, %v; want %q", state, err, testCase.want)
			}
			if testCase.want == "symlink" {
				if changed, err := installSidebarLayout(path); err != nil || changed {
					t.Fatalf("install through symlink = %t, %v", changed, err)
				}
				if data, err := os.ReadFile(source); err != nil || string(data) != testCase.content {
					t.Fatalf("managed config changed through symlink: %q, %v", data, err)
				}
			}
		})
	}
}

func TestDoctorExplainsSymlinkedSidebarConfigAndRecognizesPosseRows(t *testing.T) {
	service, _, userHome := humanSetupFixture(t)
	configHome := filepath.Join(userHome, "xdg")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(configHome, "herdr", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(userHome, "managed-herdr-config.toml")
	if err := os.WriteFile(source, []byte("[theme]\nname = 'catppuccin'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, path); err != nil {
		t.Fatal(err)
	}
	check := func() (string, string) {
		t.Helper()
		code, output := runCLI(t, service, "doctor", "--json")
		if code != 0 {
			t.Fatalf("doctor: %d %s", code, output)
		}
		var result struct {
			Checks []struct {
				Check  string `json:"check"`
				Status string `json:"status"`
				Detail string `json:"detail"`
			} `json:"checks"`
		}
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("doctor JSON: %s: %v", output, err)
		}
		for _, row := range result.Checks {
			if row.Check == "Herdr Agents sidebar layout" {
				return row.Status, row.Detail
			}
		}
		t.Fatalf("doctor omitted sidebar layout check: %s", output)
		return "", ""
	}
	status, detail := check()
	if status != "warn" || !strings.Contains(detail, "Herdr config is a symlink") || !strings.Contains(detail, sidebarLayoutSnippet) {
		t.Fatalf("symlinked config check = %q, %q", status, detail)
	}
	if err := os.WriteFile(source, []byte(sidebarLayoutSnippet), 0o600); err != nil {
		t.Fatal(err)
	}
	status, detail = check()
	if status != "ok" || detail != "Posse layout present" {
		t.Fatalf("symlink with Posse rows check = %q, %q", status, detail)
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
	if strings.Contains(output.String(), "Herdr Agents sidebar layout") {
		t.Errorf("doctor should not require the retired layout: %s", output)
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
	if code != 0 || strings.Contains(preview, "sidebar layout") || strings.Contains(preview, "$posse_row") {
		t.Fatalf("retired layout appeared in preview = %d %s", code, preview)
	}
	applied, code := runHumanSetup(t, service)
	if code != 0 || strings.Contains(applied, "sidebar layout") {
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
	if code != 0 || strings.Contains(preview, "sidebar layout") {
		t.Fatalf("preview offered retired layout = %d %s", code, preview)
	}
	applied, code := runHumanSetup(t, service, "--no-sidebar-layout")
	if code != 0 || strings.Contains(applied, "sidebar layout") {
		t.Fatalf("ordinary setup offered retired layout = %d %s", code, applied)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("layout applied without confirmation: %v", err)
	}
	adapter.RunOut["integration status"] = []byte("claude: current (v9)\ncodex: not installed\n")
	adapter.RunOut["plugin list --json"] = []byte(`{"result":{"plugins":[{"plugin_id":"posse.herdr","manifest_path":"` + filepath.Join(service.Home, "plugin", "herdr-plugin.toml") + `"}]}}`)
	preview, code = runHumanSetup(t, service, "--check", "--exit-code")
	if code != 0 || strings.Contains(preview, "sidebar layout") {
		t.Fatalf("retired layout appeared in check: %d %s", code, preview)
	}
	applied, code = runHumanSetup(t, service, "--sidebar-layout")
	if code != 2 || !strings.Contains(applied, "retired") {
		t.Fatalf("retired flag still modifies config = %d %s", code, applied)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retired flag wrote Herdr config: %v", err)
	}
	preview, code = runHumanSetup(t, service, "--check", "--exit-code")
	if code != 0 || strings.Contains(preview, "sidebar layout") {
		t.Fatalf("complete = %d %s", code, preview)
	}
}
