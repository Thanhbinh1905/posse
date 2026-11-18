package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLeadInstructionsRouteDurableUserInput(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			home := t.TempDir()
			content := ""
			if !enabled {
				content = "[knowledge]\nenabled = false\n"
			}
			if content != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.Load(home, "")
			if err != nil {
				t.Fatal(err)
			}
			result, err := composeLeadInstructions(home, store.Project{Name: "shop"}, cfg, "codex", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, phrase := range []string{"posse preferences set <lead|worker>", "harness-private memory"} {
				if !strings.Contains(result.Text, phrase) {
					t.Errorf("Lead instructions with knowledge %s omitted %q", map[bool]string{true: "enabled", false: "disabled"}[enabled], phrase)
				}
			}
			if enabled && !strings.Contains(result.Text, "posse knowledge set <id>") {
				t.Error("enabled Lead instructions omitted posse knowledge set")
			}
			if !enabled && !strings.Contains(result.Text, `posse config set knowledge.enabled true --project shop --user-approved "<User's words>"`) {
				t.Error("disabled Lead instructions did not offer a scoped, approved enable command")
			}
		})
	}
}

func TestLeadDispatchOwnsQuotaChoice(t *testing.T) {
	home := t.TempDir()
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := composeLeadInstructions(home, store.Project{Name: "shop"}, cfg, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"Read the Brief and Dispatch Rules", "--no-credential-refresh --provider claude", "Codex RPC", "unknown", "briefly state the reason", "--profile", "frozen plan", "runtime fallback"} {
		if !strings.Contains(result.Text, phrase) {
			t.Errorf("Lead dispatch instructions omitted %q", phrase)
		}
	}
}
