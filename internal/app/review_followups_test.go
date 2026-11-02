package app

import (
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLeadInstructionsDescribeRollingFollowupsAndTheCapDecision(t *testing.T) {
	home := t.TempDir()
	cfg, err := config.Load(home, "")
	if err != nil {
		t.Fatal(err)
	}
	composed, err := composeLeadInstructions(home, store.Project{Name: "shop"}, cfg, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"posse review followups", "one rolling follow-up issue per area", "review_cap_decision_required", "`accept-head`"} {
		if !strings.Contains(composed.Text, expected) {
			t.Errorf("Lead instructions omit %q", expected)
		}
	}
}
