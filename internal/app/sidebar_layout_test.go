package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRemovesExactPosseSidebarLayoutAndLeavesCustomRows(t *testing.T) {
	for index, snippet := range []string{sidebarLayoutSnippet, legacySidebarLayoutSnippet} {
		t.Run([]string{"current", "legacy"}[index], func(t *testing.T) {
			service, _, userHome := humanSetupFixture(t)
			configHome := filepath.Join(userHome, "xdg")
			t.Setenv("XDG_CONFIG_HOME", configHome)
			path := filepath.Join(configHome, "herdr", "config.toml")
			original := "# keep this comment\n" + snippet + "\n[theme]\nname = 'catppuccin'\n"
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			output, code := runHumanSetup(t, service)
			if code != 0 || !strings.Contains(output, "Posse layout removed") {
				t.Fatalf("setup = %d %s", code, output)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "# keep this comment\n\n[theme]\nname = 'catppuccin'\n" {
				t.Fatalf("config after setup = %q, %v", data, err)
			}
		})
	}

	service, _, userHome := humanSetupFixture(t)
	configHome := filepath.Join(userHome, "xdg")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(configHome, "herdr", "config.toml")
	original := "# custom\n[ui.sidebar.agents]\nrows = [[\"tab\"]]\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	output, code := runHumanSetup(t, service)
	if code != 0 || !strings.Contains(output, "custom Agents rows are now unused") {
		t.Fatalf("custom setup = %d %s", code, output)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != original {
		t.Fatalf("custom config changed = %q, %v", data, err)
	}
}

func TestSetupRejectsRemovedSidebarFlagsClearly(t *testing.T) {
	service, _, _ := humanSetupFixture(t)
	for _, flag := range []string{"--sidebar-layout", "--no-sidebar-layout"} {
		_, output := runCLI(t, service, "setup", flag)
		if !strings.Contains(output, flag+" was removed") {
			t.Errorf("%s error = %s", flag, output)
		}
	}
}

func TestDoctorNoLongerChecksHerdrSidebarLayout(t *testing.T) {
	service, _, _ := humanSetupFixture(t)
	code, output := runCLI(t, service, "doctor", "--json")
	if code != 0 {
		t.Fatalf("doctor: %d %s", code, output)
	}
	var result struct {
		Checks []struct {
			Check string `json:"check"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("doctor JSON: %s: %v", output, err)
	}
	for _, check := range result.Checks {
		if check.Check == "Herdr Agents sidebar layout" {
			t.Fatalf("doctor still checks the removed layout: %s", output)
		}
	}
}

func TestRemoveSidebarLayoutLeavesTablesExtendingThePosseSnippet(t *testing.T) {
	for name, original := range map[string]string{
		"extra key":     sidebarLayoutSnippet + "show_title = true\n",
		"trailing note": sidebarLayoutSnippet + "# mine\n",
		"subtable":      sidebarLayoutSnippet + "\n[ui.sidebar.agents.extra]\nx = 1\n",
		"inline prefix": "# a\n" + strings.Replace(sidebarLayoutSnippet, "rows = [", "rows = [\n  [\"tab\"],", 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := removeSidebarLayout(path)
			data, _ := os.ReadFile(path)
			if err != nil || result != "custom" || string(data) != original {
				t.Fatalf("result = %q, %v, config = %q", result, err, data)
			}
		})
	}
}
