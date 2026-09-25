package dispatch

import (
	"os"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
)

func TestResolveUsesSingleTypeRule(t *testing.T) {
	cfg := config.Config{
		Profiles: map[string]config.Profile{"review": {Kind: "codex"}},
		Dispatch: []config.Rule{{Type: "review", Use: "review"}},
	}
	result, err := Resolve(cfg, "review", "")
	if err != nil || result.Profile != "review" || result.Rule != "type=review" {
		t.Fatalf("Resolve() = %#v, %v", result, err)
	}
}

func TestSpecBriefFixtureParsesWithTrailingComments(t *testing.T) {
	contents, err := os.ReadFile("testdata/spec-brief.md")
	if err != nil {
		t.Fatal(err)
	}
	brief, err := ParseBriefText(string(contents))
	if err != nil {
		t.Fatalf("spec Brief fixture did not validate: %v", err)
	}
	if brief.Type != "ship" || brief.Title != "Fix flaky login test" || brief.DoneWhen != "login spec passes 20 consecutive runs in CI mode" || brief.LandingMode != "pr" || brief.ReviewOf != "" || brief.AutonomyLand != "ask" {
		t.Fatalf("parsed spec Brief = %#v", brief)
	}
}

func TestBriefRejectsRemovedNameField(t *testing.T) {
	_, err := ParseBriefText("---\ntype: ship\ntitle: Fix flaky login test\nname: worker-tree\ndone_when: tests pass\n---\n")
	briefErr, ok := err.(*BriefError)
	if !ok || briefErr.Field != "name" {
		t.Fatalf("removed name field error = %#v, want unknown name field", err)
	}
}

func TestBriefTrailingCommentsPreserveHashInsideQuotedValues(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: ship # kind\ntitle: \"Fix #42\" # title\ndone_when: 'pass # tests' # result\nautonomy: { land: ask } # policy\n---\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	if brief.Title != "Fix #42" || brief.DoneWhen != "pass # tests" || brief.AutonomyLand != "ask" {
		t.Fatalf("quoted comment values changed: %#v", brief)
	}
}

func TestAutonomyLandAutoCanBeTightenedInBrief(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: ship\ntitle: Fix\ndone_when: tests pass\nautonomy: { land: ask }\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	_, autonomy, err := ValidateTightening(brief, "pr", config.Autonomy{Land: "auto"})
	if err != nil || autonomy.Land != "ask" {
		t.Fatalf("auto Land Autonomy was not tightened to ask: %#v, %v", autonomy, err)
	}
}

func TestResolveAsksForProfileWhenWhenRulesCompete(t *testing.T) {
	cfg := config.Config{
		Profiles: map[string]config.Profile{"fast": {}, "deep": {}},
		Dispatch: []config.Rule{{When: "UI", Use: "fast"}, {When: "architecture", Use: "deep"}},
	}
	_, err := Resolve(cfg, "ship", "")
	profileError, ok := err.(*ProfileError)
	if !ok || profileError.Code != "profile_required" || len(profileError.Candidates) != 2 {
		t.Fatalf("expected Profile choice, got %#v", err)
	}
}

func TestResolveExplicitAndDefaultProfiles(t *testing.T) {
	cfg := config.Config{Profiles: map[string]config.Profile{"fast": {}}, DispatchDefault: config.DispatchDefault{Use: "fast"}}
	result, err := Resolve(cfg, "ship", "fast")
	if err != nil || result.Profile != "fast" || result.Rule != "--profile fast" {
		t.Fatalf("explicit resolution = %#v, %v", result, err)
	}
	result, err = Resolve(cfg, "ship", "")
	if err != nil || result.Profile != "fast" || result.Rule != "dispatch.default" {
		t.Fatalf("default resolution = %#v, %v", result, err)
	}
	_, err = Resolve(cfg, "ship", "missing")
	if profileError, ok := err.(*ProfileError); !ok || !strings.Contains(profileError.Message, "unknown Profile") {
		t.Fatalf("unknown Profile error = %#v", err)
	}
}

func TestBriefParsingAndTightening(t *testing.T) {
	brief, err := ParseBriefText(`---
type: ship
title: Fix login
done_when: login test passes
landing_mode: no-mistakes
autonomy: { land: ask }
---
Do the work.
`)
	if err != nil {
		t.Fatal(err)
	}
	mode, autonomy, err := ValidateTightening(brief, "pr", config.Autonomy{Review: "lead", Land: "auto"})
	if err != nil || mode != "no-mistakes" || autonomy.Land != "ask" || autonomy.Review != "lead" {
		t.Fatalf("tightened values = %q %#v, %v", mode, autonomy, err)
	}
}

func TestBriefRejectsInvalidOrLooserContract(t *testing.T) {
	for _, source := range []string{
		"---\ntitle: missing type\ndone_when: x\n---\n",
		"---\ntype: ship\ntitle: x\ndone_when: x\nunknown: x\n---\n",
		"---\ntype: review\ntitle: x\ndone_when: x\n---\n",
	} {
		if _, err := ParseBriefText(source); err == nil {
			t.Fatalf("accepted invalid Brief:\n%s", source)
		}
	}
	for _, testCase := range []struct {
		name        string
		frontmatter string
		mode        string
		autonomy    config.Autonomy
		wantField   string
	}{
		{name: "Landing Mode", frontmatter: "landing_mode: local\n", mode: "pr", autonomy: config.Autonomy{Land: "ask"}, wantField: "landing_mode"},
		{name: "Autonomy", frontmatter: "autonomy: { land: auto }\n", mode: "pr", autonomy: config.Autonomy{Land: "ask"}, wantField: "autonomy.land"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			brief, err := ParseBriefText("---\ntype: ship\ntitle: x\ndone_when: x\n" + testCase.frontmatter + "---\n")
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = ValidateTightening(brief, testCase.mode, testCase.autonomy)
			briefErr, ok := err.(*BriefError)
			if !ok || briefErr.Field != testCase.wantField {
				t.Fatalf("looser %s was not rejected: %#v", testCase.name, err)
			}
		})
	}
}
