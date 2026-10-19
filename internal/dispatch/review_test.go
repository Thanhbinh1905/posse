package dispatch

import (
	"strings"
	"testing"
)

func TestReviewPolicyBriefSelectionAndMigration(t *testing.T) {
	for _, policy := range []string{"none", "local", "no-mistakes", "always", "on_risk", "never"} {
		text := "---\ntype: change\ntitle: Policy\ndone_when: checked\nreview: " + policy + "\n---\n"
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

func TestBriefReviewRoundsAreChangeOnlyAndBounded(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: change\ntitle: Limited review\ndone_when: checked\nreview_rounds: 2\n---\n")
	if err != nil || brief.ReviewRounds != 2 {
		t.Fatalf("review_rounds parse = %#v, %v", brief, err)
	}
	for _, testCase := range []struct {
		name string
		text string
	}{
		{"zero", "review_rounds: 0"},
		{"above hard cap", "review_rounds: 4"},
		{"non-integer", "review_rounds: two"},
		{"non-Change", "type: research\nreview_rounds: 2"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := "type: change\n"
			if strings.HasPrefix(testCase.text, "type:") {
				body = ""
			}
			if _, err := ParseBriefText("---\n" + body + "title: Test\ndone_when: checked\n" + testCase.text + "\n---\n"); err == nil {
				t.Fatalf("accepted %s", testCase.text)
			}
		})
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
