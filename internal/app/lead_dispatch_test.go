package app

import (
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

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
