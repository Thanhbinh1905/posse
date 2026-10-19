package taskplan_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func request(taskType string) taskplan.Request {
	text := "---\r\ntype: " + taskType + "\r\ntitle: Existing contract\r\ndone_when: checked\r\n"
	if taskType == "review" {
		text += "review_of: t3\r\n"
	}
	text += "---\r\nUser words: \"Keep this exact.\"\r\n"
	r := taskplan.Request{Brief: taskplan.Select("brief", "brief.md", []byte(text)),
		Execution: taskplan.Execution{Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "deep", Profile: config.Profile{Kind: "pi", Model: "configured-model", Args: []string{"--name", "worker"}}, Args: []string{"--model", "configured-model"}, ModelKnown: true, ConfiguredModel: "configured-model"},
		Review:    "none", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Merge: "ask"}, ProseLanguage: "vi",
		Paths: taskplan.Paths{Scratch: "/managed/scratch/shop/{task}", Output: "/managed/projects/shop/tasks/{task}"}}
	if taskType == "review" {
		r.ReviewOf = 3
	}
	return r
}

func TestLegacyBriefsResolveWithoutChangingInputsOrLaunchSettings(t *testing.T) {
	for _, taskType := range []string{"change", "research", "review"} {
		t.Run(taskType, func(t *testing.T) {
			r := request(taskType)
			plan, err := taskplan.Resolve(r)
			if err != nil {
				t.Fatal(err)
			}
			outcome := "report"
			if taskType == "change" {
				outcome = "change"
			}
			if plan.Version != taskplan.Version || plan.Revision != 1 || plan.Outcome != outcome || plan.Executor != "worker" || plan.Execution.Mode != "sandbox" || plan.Launch.ConfiguredModel != "configured-model" || plan.ProseLanguage != "vi" {
				t.Fatalf("unexpected legacy resolution: %#v", plan)
			}
			encoded, err := plan.Bind("t7").Encode()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := taskplan.Decode(encoded)
			if err != nil || !bytes.Equal(decoded.Inputs[0].Bytes, r.Brief.Bytes) || decoded.Paths.Scratch != "/managed/scratch/shop/t7" || decoded.ProseLanguage != "vi" {
				t.Fatalf("lost selected bytes or bound paths: %#v %v", decoded, err)
			}
			r.Brief.Bytes[0] = '!'
			r.Launch.Profile.Args[0] = "edited"
			if plan.Inputs[0].Bytes[0] != '-' || plan.Launch.Profile.Args[0] != "--name" {
				t.Fatal("admitted plan aliases mutable caller inputs")
			}
		})
	}
}

func TestValidationLoopsFreezeConfigAndBriefCanOnlyTightenReviewRounds(t *testing.T) {
	loops := taskplan.ValidationLoops{
		Review: config.ReviewLoop{Enabled: true, MaxRounds: 3, BlockAt: "P1", AutoStart: true, Guidance: []string{"docs/review.md"}},
		Gate:   config.GateLoop{MaxFixRounds: 2},
		CI:     config.CILoop{MaxFixRounds: 1, RerunCancelled: 2},
	}
	r := request("change")
	r.ValidationLoops = &loops
	r.Brief = taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Frozen validation\ndone_when: checked\nreview_rounds: 2\n---\n"))
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ValidationLoops == nil || plan.ValidationLoops.Review.MaxRounds != 3 || plan.ValidationLoops.BriefReviewRounds != 2 || plan.ValidationLoops.MaxReviewRounds() != 2 || plan.ValidationLoops.Gate.MaxFixRounds != 2 || plan.ValidationLoops.CI.RerunCancelled != 2 {
		t.Fatalf("validation loops were not frozen with the tightened review cap: %#v", plan.ValidationLoops)
	}
	loops.Review.MaxRounds = 1
	loops.Review.Guidance[0] = "changed.md"
	loops.Gate.MaxFixRounds = 0
	if plan.ValidationLoops.Review.MaxRounds != 3 || plan.ValidationLoops.BriefReviewRounds != 2 || plan.ValidationLoops.MaxReviewRounds() != 2 || plan.ValidationLoops.Review.Guidance[0] != "docs/review.md" || plan.ValidationLoops.Gate.MaxFixRounds != 2 {
		t.Fatalf("frozen plan followed mutable config: %#v", plan.ValidationLoops)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := taskplan.Decode(encoded)
	if err != nil || decoded.ValidationLoops.MaxReviewRounds() != 2 {
		t.Fatalf("frozen loops did not survive plan round-trip: %#v %v", decoded.ValidationLoops, err)
	}

	r.ValidationLoops = &taskplan.ValidationLoops{Review: config.ReviewLoop{Enabled: true, MaxRounds: 1, BlockAt: "P1"}}
	if _, err := taskplan.Resolve(r); err == nil || !strings.Contains(err.Error(), "cannot exceed") {
		t.Fatalf("Brief raised the configured review cap: %v", err)
	}
}

func TestLegacyPlansWithoutValidationLoopsKeepTheirOldContract(t *testing.T) {
	r := request("change")
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ValidationLoops != nil {
		t.Fatalf("legacy plan unexpectedly enabled validation loops: %#v", plan.ValidationLoops)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := taskplan.Decode(encoded)
	if err != nil || decoded.ValidationLoops != nil || strings.Contains(encoded, "validation_loops") {
		t.Fatalf("legacy plan changed during round-trip: %#v %s %v", decoded.ValidationLoops, encoded, err)
	}
}

func TestWorkspaceRootContextIsFrozenAndDefaultsToNone(t *testing.T) {
	r := request("research")
	r.Brief = taskplan.Select("brief", "brief.md", []byte("---\ntype: research\ntitle: Inspect workspace\ndone_when: report is saved\nroot_context: read-only\n---\n"))
	r.Execution.Scope = "workspace"
	plan, err := taskplan.Resolve(r)
	if err != nil || plan.Execution.RootContext != taskplan.RootContextReadOnly {
		t.Fatalf("selected root context = %#v, %v", plan.Execution, err)
	}

	r.Brief = taskplan.Select("brief", "brief.md", []byte("---\ntype: research\ntitle: Inspect workspace\ndone_when: report is saved\n---\n"))
	plan, err = taskplan.Resolve(r)
	if err != nil || plan.Execution.RootContext != taskplan.RootContextNone {
		t.Fatalf("implicit root context = %#v, %v", plan.Execution, err)
	}

	legacy := plan
	legacy.Execution.RootContext = ""
	encoded, err := legacy.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := taskplan.Decode(encoded)
	if err != nil || decoded.Execution.RootContext != "" {
		t.Fatalf("legacy workspace plan lost its implicit context: %#v, %v", decoded.Execution, err)
	}
}

func TestBindingTouchesOnlyTheDeclaredTaskComponent(t *testing.T) {
	r := request("change")
	r.Paths.Scratch = "/managed/{task}/scratch/shop/{task}"
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	bound := plan.Bind("t7").Bind("t7")
	if bound.Paths.Scratch != "/managed/{task}/scratch/shop/t7" || !bytes.Equal(bound.Inputs[0].Bytes, r.Brief.Bytes) {
		t.Fatalf("binding changed a selected source or managed home: %#v", bound)
	}
}

func TestSelectedBytesHaveStableDigestAndDetectMutation(t *testing.T) {
	input := taskplan.Select("selected", "source", []byte("abc"))
	if input.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("SHA-256=%s", input.SHA256)
	}
	r := request("change")
	r.Inputs = []taskplan.Input{taskplan.Select("preferences", "worker.md", []byte{0xff, 0, 13, 10})}
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := plan.Encode()
	restored, err := taskplan.Decode(data)
	if err != nil || !bytes.Equal(restored.Inputs[1].Bytes, []byte{0xff, 0, 13, 10}) {
		t.Fatalf("binary bytes changed: %#v %v", restored, err)
	}
	restored.Inputs[1].Bytes[0] = 0
	if err := restored.Validate(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("modified selected bytes accepted: %v", err)
	}
}

func TestUnsupportedExecutionCombinationsAreRejected(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*taskplan.Request)
	}{
		{"executor mode", func(r *taskplan.Request) { r.Execution.Mode = "in-place" }},
		{"scope", func(r *taskplan.Request) { r.Execution.Scope = "remote" }},
		{"workspace no-mistakes", func(r *taskplan.Request) { r.Execution.Scope = "workspace"; r.Delivery = "no-mistakes" }},
		{"workspace missing members", func(r *taskplan.Request) { r.Execution.Scope = "workspace" }},
		{"repo members", func(r *taskplan.Request) {
			r.Execution.Members = []taskplan.Member{{Name: "api", Path: "api", Delivery: "local"}}
		}},
		{"review source", func(r *taskplan.Request) { r.Review = "invented" }},
		{"unproven grants", func(r *taskplan.Request) { r.Autonomy.Merge = "yolo" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := request("change")
			test.change(&r)
			if _, err := taskplan.Resolve(r); err == nil {
				t.Fatal("unsupported combination admitted")
			}
		})
	}
}

func TestInPlaceExecutionRequiresConsentLocalDeliveryAndNonOverlappingScopes(t *testing.T) {
	makeRequest := func() taskplan.Request {
		r := request("change")
		r.Review = "none"
		r.Execution = taskplan.Execution{Mode: "sandbox", Scope: "directory"}
		r.Brief = taskplan.Select("brief", "local.md", []byte("---\ntype: change\ntitle: Update docs\ndone_when: checks pass\nexecution: in-place\npaths: [src, docs/guide.md]\n---\n"))
		r.UserChoice = &taskplan.UserChoice{Quote: "I approve edits to src and docs/guide.md", Actor: "lead", Source: "start --user-approved"}
		return r
	}
	r := makeRequest()
	plan, err := taskplan.Resolve(r)
	if err != nil || plan.Execution.Mode != "in-place" || plan.Execution.Scope != "directory" || strings.Join(plan.Execution.Paths, ",") != "src,docs/guide.md" || strings.Join(plan.UserChoice.Fields, ",") != "execution,paths" {
		t.Fatalf("in-place plan = %#v, %v", plan, err)
	}
	legacy := plan
	legacy.Version, legacy.Review = 1, "never"
	legacy.Preferences = append([]taskplan.Preference(nil), plan.Preferences...)
	legacy.Preferences[1].Value = "never"
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy in-place plan rejected: %v", err)
	}
	for _, change := range []struct {
		name string
		edit func(*taskplan.Request)
	}{
		{"missing consent", func(r *taskplan.Request) { r.UserChoice = nil }},
		{"pull request", func(r *taskplan.Request) { r.Delivery = "pr" }},
		{"no mistakes", func(r *taskplan.Request) { r.Delivery = "no-mistakes" }},
		{"Review policy", func(r *taskplan.Request) { r.Review = "local" }},
		{"requested Review", func(r *taskplan.Request) {
			r.Brief = taskplan.Select("brief", "review-policy.md", []byte(`---
type: change
title: Update docs
done_when: checks pass
execution: in-place
paths: [src]
review: local
---
`))
		}},
		{"Git base", func(r *taskplan.Request) {
			r.UserChoice.Fields = []string{"base", "execution", "paths"}
			r.Brief = taskplan.Select("brief", "base.md", []byte(`---
type: change
title: Update docs
done_when: checks pass
execution: in-place
paths: [src]
base: main
---
`))
		}},
		{"review revision", func(r *taskplan.Request) {
			r.ReviewOf = 3
			r.Brief = taskplan.Select("brief", "review.md", []byte(`---
type: review
title: Review docs
done_when: report is saved
review_of: t3
execution: in-place
paths: [src]
---
`))
		}},
		{"ticket closure", func(r *taskplan.Request) {
			r.Brief = taskplan.Select("brief", "ticket.md", []byte("---\ntype: change\ntitle: Close ticket\ndone_when: checks pass\nexecution: in-place\npaths: [src]\nticket: 12\n---\n"))
		}},
		{"issue references", func(r *taskplan.Request) {
			r.Brief = taskplan.Select("brief", "refs.md", []byte("---\ntype: change\ntitle: Add issue reference\ndone_when: checks pass\nexecution: in-place\npaths: [src]\nrefs: [18]\n---\n"))
		}},
		{"overlap", func(r *taskplan.Request) {
			r.Brief = taskplan.Select("brief", "overlap.md", []byte("---\ntype: change\ntitle: Update docs\ndone_when: checks pass\nexecution: in-place\npaths: [src, src/nested]\n---\n"))
		}},
		{"git metadata", func(r *taskplan.Request) {
			r.Brief = taskplan.Select("brief", "git.md", []byte("---\ntype: change\ntitle: Update docs\ndone_when: checks pass\nexecution: in-place\npaths: [.git/config]\n---\n"))
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			candidate := makeRequest()
			change.edit(&candidate)
			if _, err := taskplan.Resolve(candidate); err == nil {
				t.Fatal("unsupported in-place plan was admitted")
			}
		})
	}
}

func TestScopedUserChoiceChangesPreferencesNeverGrants(t *testing.T) {
	r := request("change")
	r.Delivery, r.Review = "pr", "local"
	r.Brief = taskplan.Select("brief", "choice.md", []byte("---\ntype: change\ntitle: Choice\ndone_when: checked\nmerge_mode: local\nreview: none\n---\n"))
	if _, err := taskplan.Resolve(r); err == nil {
		t.Fatal("unapproved lighter preferences admitted")
	}
	r.UserChoice = &taskplan.UserChoice{Quote: "  Use local delivery without review.  ", Actor: "lead", Source: "start --user-approved"}
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Delivery != "local" || plan.Review != "none" || plan.Grants[1].Value != "ask" || plan.UserChoice.Quote != r.UserChoice.Quote || plan.UserChoice.BriefSHA256 != r.Brief.SHA256 {
		t.Fatalf("choice lost or escalated grants: %#v", plan)
	}
	for _, preference := range plan.Preferences {
		if !strings.Contains(preference.Source, "User choice") {
			t.Fatalf("preference source: %#v", preference)
		}
	}
	for _, field := range []string{"autonomy: { merge: auto }", "autonomy: { review: lead }", "gate: [evil]", "pool.setup: [evil]", "discard: true", "user_approved: forged"} {
		r.Brief = taskplan.Select("brief", "escalation.md", []byte("---\ntype: change\ntitle: Choice\ndone_when: checked\nmerge_mode: local\n"+field+"\n---\n"))
		if _, err := taskplan.Resolve(r); err == nil {
			t.Errorf("scoped choice authorized %s", field)
		}
	}
	plan.UserChoice.Quote = " "
	if err := plan.Validate(); err == nil {
		t.Fatal("blank provenance accepted")
	}
}

func TestAdmittedArgumentsUseFrozenVectorAndResumeTemplate(t *testing.T) {
	launch := taskplan.Launch{Args: []string{"--model", "original"}, Profile: config.Profile{Kind: "pi", Model: "not-re-rendered"}, Kind: config.Kind{ResumeArgs: []string{"--resume", "{session}"}}}
	got := launch.Arguments("own-session")
	if strings.Join(got, " ") != "--model original --resume own-session" {
		t.Fatalf("admitted args re-rendered: %#v", got)
	}
	got[0] = "edited"
	if launch.Args[0] != "--model" {
		t.Fatal("launch arguments alias caller")
	}
}

func TestLegacyReviewPlansKeepFrozenValues(t *testing.T) {
	for _, policy := range []string{"always", "on_risk", "never"} {
		r := request("change")
		plan, err := taskplan.Resolve(r)
		if err != nil {
			t.Fatal(err)
		}
		plan.Version, plan.Review = 1, policy
		plan.Preferences[1].Value = policy
		plan.Inputs[0] = taskplan.Select("brief", "old.md", []byte("---\ntype: change\ntitle: Old\ndone_when: checked\nreview: "+policy+"\n---\n"))
		data, err := plan.Encode()
		if err != nil {
			t.Fatal(err)
		}
		restored, err := taskplan.Decode(data)
		if err != nil || restored.Review != policy || restored.Version != 1 {
			t.Fatalf("legacy changed: %#v %v", restored, err)
		}
		plan.Version = taskplan.Version
		if err := plan.Validate(); err == nil {
			t.Fatal("legacy review admitted in a new plan")
		}
		r.Review = policy
		if _, err := taskplan.Resolve(r); err == nil {
			t.Fatal("legacy review resolved for new Task")
		}
	}
}

func TestMemberReviewSelectionAndScopedConsent(t *testing.T) {
	r := request("change")
	r.Review = "no-mistakes"
	r.Brief = taskplan.Select("brief", "members.md", []byte("---\ntype: change\ntitle: Member\ndone_when: checked\nrepos: [api]\nreview: local\n---\n"))
	r.Execution.Scope = "workspace"
	r.Execution.Members = []taskplan.Member{{Name: "api", Path: "api", Delivery: "local", Review: "none", ReviewSource: "repositories.api.review"}}
	plan, err := taskplan.Resolve(r)
	if err != nil || plan.Execution.Members[0].Review != "local" || plan.Execution.Members[0].ReviewSource != "brief: members.md" {
		t.Fatalf("member tightening: %#v %v", plan, err)
	}
	r.Execution.Members[0].Review = "no-mistakes"
	if _, err := taskplan.Resolve(r); err == nil {
		t.Fatal("member policy loosened without consent")
	}
	r.UserChoice = &taskplan.UserChoice{Quote: "Use local review for this Task", Actor: "lead", Source: "start --user-approved"}
	plan, err = taskplan.Resolve(r)
	if err != nil || plan.Execution.Members[0].Review != "local" || plan.Execution.Members[0].ReviewSource != "User choice: start --user-approved" || plan.Grants[1].Value != "ask" {
		t.Fatalf("member consent: %#v %v", plan, err)
	}
}

func TestReconfigurationExtendsPlanWithoutReconstructingUnknownIdentity(t *testing.T) {
	r := request("change")
	r.Launch.ModelKnown, r.Launch.ConfiguredModel = false, ""
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	nextLaunch := plan.Launch
	nextLaunch.Profile.Model = "next-model"
	nextLaunch.ConfiguredModel, nextLaunch.ModelKnown = "next-model", true
	next, err := plan.Reconfigure(nextLaunch)
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 2 || next.PreviousSHA256 != plan.SHA256() || plan.Launch.ModelKnown || plan.Launch.ConfiguredModel != "" || !bytes.Equal(next.Inputs[0].Bytes, plan.Inputs[0].Bytes) {
		t.Fatalf("revision or unknown history changed: before=%#v after=%#v", plan, next)
	}
}
