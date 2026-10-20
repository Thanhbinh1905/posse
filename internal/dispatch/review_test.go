package dispatch

import (
	"strings"
	"testing"
)

func TestReviewPolicyBriefSelectionAndMigration(t *testing.T) {
	for _, policy := range []string{"none", "quick", "full-pipeline", "no-mistakes", "local", "always", "on_risk", "never"} {
		text := "---\ntype: change\ntitle: Policy\ndone_when: checked\nreview: " + policy + "\n---\n"
		brief, err := ParseBriefText(text)
		legacy := policy == "always" || policy == "on_risk" || policy == "never"
		switch {
		case policy == "local":
			if err == nil || !strings.Contains(err.Error(), "use quick") {
				t.Fatalf("local alias admitted: %v", err)
			}
		case legacy:
			if err == nil || !strings.Contains(err.Error(), "legacy review policy") {
				t.Fatalf("legacy admitted: %s %v", policy, err)
			}
			if historical, err := ParseHistoricalBriefText(text); err != nil || historical.Review != policy {
				t.Fatalf("historical Brief changed: %#v %v", historical, err)
			}
		default:
			if err != nil || brief.Review != policy {
				t.Fatalf("policy refused: %#v %v", brief, err)
			}
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
		{"none", "", false, true}, {"none", "quick", false, true}, {"quick", "full-pipeline", false, true},
		{"quick", "none", false, false}, {"full-pipeline", "quick", false, false}, {"no-mistakes", "quick", false, false},
		{"quick", "none", true, true}, {"no-mistakes", "quick", true, true}, {"full-pipeline", "quick", true, true},
		{"full-pipeline", "no-mistakes", false, false}, {"full-pipeline", "no-mistakes", true, true},
		{"always", "none", true, false}, {"local", "quick", true, false},
		{"none", "local", false, false}, {"none", "local", true, false}, {"none", "invented", true, false},
	} {
		got, err := ResolveReview(tc.requested, tc.current, tc.consent)
		if (err == nil) != tc.allowed {
			t.Errorf("%+v: %q %v", tc, got, err)
		}
	}
}
