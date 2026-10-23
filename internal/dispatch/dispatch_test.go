package dispatch

import (
	"os"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
)

func TestWorkflowRunBriefFields(t *testing.T) {
	valid := "---\ntype: research\ntitle: Continue workflow\ndone_when: Report complete\nworkflow_run: w12\nstage: Plan\n---\n"
	brief, err := ParseBriefText(valid)
	if err != nil || brief.WorkflowRun != "w12" || brief.Stage != "Plan" {
		t.Fatalf("parse workflow continuation = %#v, %v", brief, err)
	}
	for _, frontmatter := range []string{
		"workflow_run: w1\n",
		"stage: Build\n",
		"workflow_run: workflow-1\nstage: Build\n",
		"workflow_run: w1\nstage:   \n",
	} {
		contents := "---\ntype: change\ntitle: Invalid workflow\ndone_when: checked\n" + frontmatter + "---\n"
		if _, err := ParseBriefText(contents); err == nil {
			t.Errorf("invalid workflow Brief accepted: %q", frontmatter)
		}
	}
}

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

func TestImplicitInheritanceDefaultsToKindAndCanUseLeadProfile(t *testing.T) {
	cfg := config.Config{
		Lead:     config.Lead{Kind: "claude", Profiles: map[string]string{"claude": "lead"}},
		Defaults: config.Defaults{DispatchInheritance: "kind"},
		Kinds:    map[string]config.Kind{"claude": {}},
		Profiles: map[string]config.Profile{"lead": {Kind: "claude", Model: "model-a", Effort: "high"}},
	}
	result, err := Resolve(cfg, "change", "")
	if err != nil || result.Profile != "implicit:claude" || result.Rule != ImplicitRule || result.Resolved.Model != "" || result.Resolved.Effort != "" {
		t.Fatalf("default kind-only inheritance = %#v, %v", result, err)
	}
	cfg.Defaults.DispatchInheritance = "lead_profile"
	result, err = Resolve(cfg, "change", "")
	if err != nil || result.Profile != "lead" || result.Rule != ImplicitLeadProfileRule || result.Resolved.Model != "model-a" || result.Resolved.Effort != "high" {
		t.Fatalf("Lead Profile inheritance = %#v, %v", result, err)
	}
}

func TestDisabledKindsRefuseNewDispatchButRemainReadable(t *testing.T) {
	cfg := config.Config{
		Lead:     config.Lead{Kind: "claude"},
		Defaults: config.Defaults{DispatchInheritance: "kind"},
		Kinds:    map[string]config.Kind{"claude": {Disabled: true}},
		Profiles: map[string]config.Profile{"worker": {Kind: "claude"}, "other": {Kind: "codex"}},
	}
	for _, testCase := range []struct {
		name           string
		requested      string
		defaultProfile string
		wantCode       string
	}{
		{name: "implicit Lead kind", wantCode: "kind_disabled"},
		{name: "explicit Profile", requested: "worker", wantCode: "kind_disabled"},
		{name: "configured default", defaultProfile: "worker", wantCode: "kind_disabled"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg.DispatchDefault.Use = testCase.defaultProfile
			requested := testCase.requested
			if testCase.name == "implicit Lead kind" {
				cfg.DispatchDefault.Use = ""
			}
			_, err := Resolve(cfg, "change", requested)
			profileError, ok := err.(*ProfileError)
			if !ok || profileError.Code != testCase.wantCode {
				t.Fatalf("Resolve error = %#v, want %s", err, testCase.wantCode)
			}
		})
	}
	for _, rule := range []string{ImplicitRule, ImplicitLeadProfileRule} {
		profile, ok := StoredProfile(cfg, "implicit:claude", rule)
		if !ok || profile.Kind != "claude" {
			t.Errorf("disabled historical Profile with rule %q = %#v, %t", rule, profile, ok)
		}
	}
	profile, ok := StoredProfile(cfg, "worker", "--profile worker")
	if !ok || profile.Kind != "claude" {
		t.Fatalf("disabled named historical Profile = %#v, %t", profile, ok)
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
	if brief.Type != "change" || brief.Title != "Fix flaky login test" || brief.DoneWhen != "login spec passes 20 consecutive runs in CI mode" || brief.MergeMode != "pr" || brief.ReviewOf != "" || brief.AutonomyMerge != "ask" {
		t.Fatalf("parsed spec Brief = %#v", brief)
	}
}

func TestBriefParsesCanonicalAndLegacyTicketFields(t *testing.T) {
	for _, frontmatter := range []string{
		"ticket: 12\n",
		"issues: [12]\n",
		"ticket: 12\nissues: [12]\n",
	} {
		source := "---\ntype: change\ntitle: Fix issue links\ndone_when: PR body includes links\n" + frontmatter + "refs: [worker#9]\n---\nWork on the listed issue.\n"
		brief, err := ParseBriefText(source)
		if err != nil {
			t.Fatalf("Brief with ticket %q did not validate: %v", frontmatter, err)
		}
		if brief.Ticket == nil || *brief.Ticket != (IssueRef{Number: 12}) {
			t.Fatalf("parsed ticket = %#v for %q", brief.Ticket, frontmatter)
		}
		if len(brief.Refs) != 1 || brief.Refs[0].Repository != "worker" || brief.Refs[0].Number != 9 {
			t.Fatalf("parsed refs = %#v", brief.Refs)
		}
	}
}

func TestLegacyWorkspaceIssueAliasBecomesTheCanonicalTicket(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: change\ntitle: Workspace issue\ndone_when: member is linked\nissues: [worker#12]\n---\n")
	if err != nil {
		t.Fatalf("legacy Workspace alias did not parse: %v", err)
	}
	if brief.Ticket == nil || *brief.Ticket != (IssueRef{Repository: "worker", Number: 12}) {
		t.Fatalf("legacy Workspace issue alias = %#v", brief.Ticket)
	}
}

func TestBriefParserRejectsConflictingOrMultipleClosingIssues(t *testing.T) {
	for _, frontmatter := range []string{
		"ticket: 12\nissues: [13]\n",
		"ticket: 12\nissues: [12, 13]\n",
		"issues: [12, 13]\n",
		"ticket: [12]\n",
		"ticket: 12, 13\n",
	} {
		source := "---\ntype: change\ntitle: Fix issue links\ndone_when: tests pass\n" + frontmatter + "---\n"
		if _, err := ParseBriefText(source); err == nil {
			t.Fatalf("accepted invalid canonical ticket:\n%s", source)
		}
	}
}

func TestHistoricalBriefParserPreservesMultipleLegacyIssues(t *testing.T) {
	brief, err := ParseHistoricalBriefText("---\ntype: change\ntitle: Old multi-issue Task\ndone_when: historical\nissues: [12, 16]\n---\n")
	if err != nil {
		t.Fatalf("historical Brief did not parse: %v", err)
	}
	if brief.Ticket != nil || len(brief.Issues) != 2 {
		t.Fatalf("historical issue references changed: %#v", brief)
	}
}

func TestBriefRejectsInvalidOrRepeatedIssueReferences(t *testing.T) {
	for _, frontmatter := range []string{
		"issues: [0]\n",
		"issues: [12, 12]\n",
		"issues: [member#12]\nrefs: [member#12]\n",
		"refs: [../member#12]\n",
	} {
		source := "---\ntype: change\ntitle: Fix issue links\ndone_when: tests pass\n" + frontmatter + "---\n"
		if _, err := ParseBriefText(source); err == nil {
			t.Fatalf("accepted invalid issue references:\\n%s", source)
		}
	}
}

func TestBriefParsesTypedTaskDependencies(t *testing.T) {
	brief, err := ParseBriefText(`---
type: research
title: Consumer
done_when: consume input
after: [t12:accepted, t14:reported]
---
`)
	if err != nil || len(brief.After) != 2 || brief.After[0] != (Dependency{TaskID: "t12", Milestone: "accepted"}) || brief.After[1] != (Dependency{TaskID: "t14", Milestone: "reported"}) {
		t.Fatalf("typed dependencies=%#v err=%v", brief.After, err)
	}
}

func TestBriefRejectsInvalidTypedTaskDependencies(t *testing.T) {
	for _, value := range []string{"t0:landed", "t12:done", "t12:landed,t12:reported", "t12"} {
		if _, err := ParseBriefText("---\ntype: change\ntitle: Consumer\ndone_when: consume input\nafter: " + value + "\n---\n"); err == nil {
			t.Errorf("accepted invalid after value %q", value)
		}
	}
}

func TestBriefParsesPublicationBoundReportKind(t *testing.T) {
	for _, kind := range []string{"spec", "tickets"} {
		source := "---\ntype: research\ntitle: Planning Report\ndone_when: findings are complete\nreport_kind: " + kind + "\n---\n"
		brief, err := ParseBriefText(source)
		if err != nil || brief.ReportKind != kind {
			t.Fatalf("report_kind %q parsed as %#v, err=%v", kind, brief, err)
		}
	}
}

func TestBriefRejectsInvalidReportKindPlacement(t *testing.T) {
	for _, source := range []string{
		"---\ntype: change\ntitle: Change\ndone_when: done\nreport_kind: spec\n---\n",
		"---\ntype: review\ntitle: Review\ndone_when: done\nreport_kind: tickets\nreview_of: t1\n---\n",
		"---\ntype: research\ntitle: Research\ndone_when: done\nreport_kind: draft\n---\n",
	} {
		if _, err := ParseBriefText(source); err == nil {
			t.Fatalf("accepted invalid report_kind Brief:\\n%s", source)
		}
	}
}

func TestBriefRejectsRemovedNameField(t *testing.T) {
	_, err := ParseBriefText("---\ntype: change\ntitle: Fix flaky login test\nname: worker-tree\ndone_when: tests pass\n---\n")
	briefErr, ok := err.(*BriefError)
	if !ok || briefErr.Field != "name" {
		t.Fatalf("removed name field error = %#v, want unknown name field", err)
	}
}

func TestBriefTrailingCommentsPreserveHashInsideQuotedValues(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: change # kind\ntitle: \"Fix #42\" # title\ndone_when: 'pass # tests' # result\nautonomy: { merge: ask } # policy\n---\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	if brief.Title != "Fix #42" || brief.DoneWhen != "pass # tests" || brief.AutonomyMerge != "ask" {
		t.Fatalf("quoted comment values changed: %#v", brief)
	}
}

func TestBriefParsesTaskBaseAndTargetBranches(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: change\ntitle: Stacked change\ndone_when: published\nbase: posse/lower-task\ntarget: {api: staging, worker: posse/lower-task}\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if brief.Base != "posse/lower-task" || brief.Target != "" || len(brief.TargetByRepo) != 2 || brief.TargetByRepo["api"] != "staging" || brief.TargetByRepo["worker"] != "posse/lower-task" {
		t.Fatalf("parsed branch selections = %#v", brief)
	}
}

func TestBriefExplainsYAMLListFrontmatter(t *testing.T) {
	for _, item := range []string{"tests pass", `"Root cause at a_test.go:178 is identified"`, "key: value"} {
		source := "---\ntype: change\ntitle: List value\ndone_when: ok\nrefs:\n  - " + item + "\n---\n"
		_, err := ParseBriefText(source)
		briefErr, ok := err.(*BriefError)
		if !ok || briefErr.Field != "frontmatter" || !strings.Contains(briefErr.Reason, "YAML lists are unsupported") {
			t.Fatalf("YAML list error for %q = %#v", item, err)
		}
	}
}

func TestBriefParsesDoneWhenList(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: change\ntitle: List\ndone_when:\n  - tests pass\n  - \"Root cause at a_test.go:178 is identified\" # note\n  -   'lint is clean'\nmerge_mode: pr\n---\nbody\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "tests pass; Root cause at a_test.go:178 is identified; lint is clean"; brief.DoneWhen != want {
		t.Fatalf("DoneWhen = %q, want %q", brief.DoneWhen, want)
	}
	if brief.MergeMode != "pr" || brief.Body != "body" {
		t.Fatalf("fields after list = %#v", brief)
	}
}

func TestBriefRejectsInvalidDoneWhenList(t *testing.T) {
	for _, frontmatter := range []string{
		"done_when:\n",
		"done_when:\ntitle: x\n",
		"done_when:\n  -\n",
		"done_when: tests pass\n  - extra\n",
		"done_when:\n  - a\ndone_when: b\n",
	} {
		source := "---\ntype: research\ntitle: List\n" + frontmatter + "---\n"
		if _, err := ParseBriefText(source); err == nil {
			t.Fatalf("accepted invalid done_when:\n%s", source)
		}
	}
}

func TestBriefAcceptsHashPrefixedIssueReferences(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: change\ntitle: Hash refs\ndone_when: ok\nticket: \"#439\"\nrefs: [\"#12\", member#16, 18]\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if brief.Ticket == nil || *brief.Ticket != (IssueRef{Number: 439}) {
		t.Fatalf("ticket = %#v", brief.Ticket)
	}
	want := []IssueRef{{Number: 12}, {Repository: "member", Number: 16}, {Number: 18}}
	if len(brief.Refs) != len(want) {
		t.Fatalf("refs = %#v", brief.Refs)
	}
	for i := range want {
		if brief.Refs[i] != want[i] {
			t.Fatalf("refs = %#v", brief.Refs)
		}
	}
	if _, err := ParseBriefText("---\ntype: change\ntitle: Dup\ndone_when: ok\nticket: \"#439\"\nrefs: [439]\n---\n"); err == nil {
		t.Fatal("accepted #439 duplicated as 439 in refs")
	}
	if _, err := ParseBriefText("---\ntype: change\ntitle: Bad\ndone_when: ok\nticket: \"#\"\n---\n"); err == nil {
		t.Fatal("accepted bare #")
	}
}

func TestAutonomyMergeAutoCanBeTightenedInBrief(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: change\ntitle: Fix\ndone_when: tests pass\nautonomy: { merge: ask }\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	_, autonomy, err := ValidateTightening(brief, "pr", config.Autonomy{Merge: "auto"})
	if err != nil || autonomy.Merge != "ask" {
		t.Fatalf("auto Merge Autonomy was not tightened to ask: %#v, %v", autonomy, err)
	}
}

func TestResolveAsksForProfileWhenWhenRulesCompete(t *testing.T) {
	cfg := config.Config{
		Profiles: map[string]config.Profile{"fast": {}, "deep": {}},
		Dispatch: []config.Rule{{When: "UI", Use: "fast"}, {When: "architecture", Use: "deep"}},
	}
	_, err := Resolve(cfg, "change", "")
	profileError, ok := err.(*ProfileError)
	if !ok || profileError.Code != "profile_required" || len(profileError.Candidates) != 2 {
		t.Fatalf("expected Profile choice, got %#v", err)
	}
}

func TestResolveExplicitAndDefaultProfiles(t *testing.T) {
	cfg := config.Config{Profiles: map[string]config.Profile{"fast": {}}, DispatchDefault: config.DispatchDefault{Use: "fast"}}
	result, err := Resolve(cfg, "change", "fast")
	if err != nil || result.Profile != "fast" || result.Rule != "--profile fast" {
		t.Fatalf("explicit resolution = %#v, %v", result, err)
	}
	result, err = Resolve(cfg, "change", "")
	if err != nil || result.Profile != "fast" || result.Rule != "dispatch.default" {
		t.Fatalf("default resolution = %#v, %v", result, err)
	}
	_, err = Resolve(cfg, "change", "missing")
	if profileError, ok := err.(*ProfileError); !ok || !strings.Contains(profileError.Message, "unknown Profile") {
		t.Fatalf("unknown Profile error = %#v", err)
	}
}

func TestBriefRootContextMustBeExplicitAndReadOnly(t *testing.T) {
	brief, err := ParseBriefText("---\ntype: research\ntitle: Read workspace context\ndone_when: report is saved\nroot_context: read-only\n---\n")
	if err != nil || brief.RootContext != "read-only" {
		t.Fatalf("read-only root context = %#v, %v", brief, err)
	}
	for _, value := range []string{"write", "true", "read-write"} {
		source := "---\ntype: research\ntitle: Read workspace context\ndone_when: report is saved\nroot_context: " + value + "\n---\n"
		if _, err := ParseBriefText(source); err == nil {
			t.Errorf("accepted root_context %q", value)
		}
	}
}

func TestBriefRejectsRemovedInPlaceFields(t *testing.T) {
	for _, field := range []string{"execution: in-place\n", "paths: [src]\n"} {
		source := "---\ntype: change\ntitle: Update notes\ndone_when: check passes\n" + field + "---\n"
		if _, err := ParseBriefText(source); err == nil || !strings.Contains(err.Error(), "removed") {
			t.Errorf("expected removed-feature error for %q, got %v", field, err)
		}
	}
}

func TestBriefParsingAndTightening(t *testing.T) {
	brief, err := ParseBriefText(`---
type: change
title: Fix login
done_when: login test passes
merge_mode: no-mistakes
autonomy: { merge: ask }
---
Do the work.
`)
	if err != nil {
		t.Fatal(err)
	}
	mode, autonomy, err := ValidateTightening(brief, "pr", config.Autonomy{Review: "lead", Merge: "auto"})
	if err != nil || mode != "no-mistakes" || autonomy.Merge != "ask" || autonomy.Review != "lead" {
		t.Fatalf("tightened values = %q %#v, %v", mode, autonomy, err)
	}
}

func TestBriefRejectsInvalidOrLooserContract(t *testing.T) {
	for _, source := range []string{
		"---\ntitle: missing type\ndone_when: x\n---\n",
		"---\ntype: change\ntitle: x\ndone_when: x\nunknown: x\n---\n",
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
		{name: "Merge Mode", frontmatter: "merge_mode: local\n", mode: "pr", autonomy: config.Autonomy{Merge: "ask"}, wantField: "merge_mode"},
		{name: "Autonomy", frontmatter: "autonomy: { merge: auto }\n", mode: "pr", autonomy: config.Autonomy{Merge: "ask"}, wantField: "autonomy.merge"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			brief, err := ParseBriefText("---\ntype: change\ntitle: x\ndone_when: x\n" + testCase.frontmatter + "---\n")
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
