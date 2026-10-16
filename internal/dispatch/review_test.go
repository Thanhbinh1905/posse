package dispatch

import (
	"strings"
	"testing"
)

func TestReviewPolicyBriefSelectionAndMigration(t *testing.T) {
	for _, policy := range []string{"none", "local", "no-mistakes", "always", "on_risk", "never"} {
		text := "---\ntype: ship\ntitle: Policy\ndone_when: checked\nreview: " + policy + "\n---\n"
		brief, err := ParseBriefText(text)
		legacy := policy == "always" || policy == "on_risk" || policy == "never"
		if legacy {
			if err == nil || !strings.Contains(err.Error(), "legacy review policy") {
				t.Fatalf("legacy admitted: %s %v", policy, err)
			}
			if historical, err := ParseHistoricalBriefText(text); err != nil || historical.Review != policy {
				t.Fatalf("historical Brief changed: %#v %v", historical, err)
			}
		} else if err != nil || brief.Review != policy {
			t.Fatalf("policy refused: %#v %v", brief, err)
		}
	}
}

func TestReviewPolicyTighteningAndScopedChoice(t *testing.T) {
	for _, tc := range []struct {
		current, requested string
		consent, allowed   bool
	}{
		{"none", "", false, true}, {"none", "local", false, true}, {"local", "no-mistakes", false, true},
		{"local", "none", false, false}, {"no-mistakes", "local", false, false},
		{"local", "none", true, true}, {"no-mistakes", "local", true, true},
		{"always", "none", true, false}, {"none", "invented", true, false},
	} {
		got, err := ResolveReview(tc.requested, tc.current, tc.consent)
		if (err == nil) != tc.allowed {
			t.Errorf("%+v: %q %v", tc, got, err)
		}
	}
}
