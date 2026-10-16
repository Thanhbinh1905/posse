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
		Launch:    taskplan.Launch{ProfileName: "deep", Profile: config.Profile{Kind: "pi", Model: "configured-model", Args: []string{"--name", "rider"}}, Args: []string{"--model", "configured-model"}, ModelKnown: true, ConfiguredModel: "configured-model"},
		Review:    "on_risk", Delivery: "local", Autonomy: config.Autonomy{Review: "ask", Land: "ask"},
		Paths: taskplan.Paths{Scratch: "/managed/scratch/shop/{task}", Output: "/managed/projects/shop/tasks/{task}"}}
	if taskType == "review" {
		r.ReviewOf = 3
	}
	return r
}

func TestLegacyBriefsResolveWithoutChangingInputsOrLaunchSettings(t *testing.T) {
	for _, taskType := range []string{"ship", "scout", "review"} {
		t.Run(taskType, func(t *testing.T) {
			r := request(taskType)
			plan, err := taskplan.Resolve(r)
			if err != nil {
				t.Fatal(err)
			}
			outcome := "report"
			if taskType == "ship" {
				outcome = "change"
			}
			if plan.Version != 1 || plan.Revision != 1 || plan.Outcome != outcome || plan.Executor != "rider" || plan.Execution.Mode != "mount" || plan.Launch.ConfiguredModel != "configured-model" {
				t.Fatalf("unexpected legacy resolution: %#v", plan)
			}
			encoded, err := plan.Bind("t7").Encode()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := taskplan.Decode(encoded)
			if err != nil || !bytes.Equal(decoded.Inputs[0].Bytes, r.Brief.Bytes) || decoded.Paths.Scratch != "/managed/scratch/shop/t7" {
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

func TestWorkspaceRootContextIsFrozenAndDefaultsToNone(t *testing.T) {
	r := request("scout")
	r.Brief = taskplan.Select("brief", "brief.md", []byte("---\ntype: scout\ntitle: Inspect workspace\ndone_when: report is saved\nroot_context: read-only\n---\n"))
	r.Execution.Scope = "workspace"
	plan, err := taskplan.Resolve(r)
	if err != nil || plan.Execution.RootContext != taskplan.RootContextReadOnly {
		t.Fatalf("selected root context = %#v, %v", plan.Execution, err)
	}

	r.Brief = taskplan.Select("brief", "brief.md", []byte("---\ntype: scout\ntitle: Inspect workspace\ndone_when: report is saved\n---\n"))
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
	r := request("ship")
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
	r := request("ship")
	r.Inputs = []taskplan.Input{taskplan.Select("preferences", "rider.md", []byte{0xff, 0, 13, 10})}
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
		{"unproven grants", func(r *taskplan.Request) { r.Autonomy.Land = "yolo" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := request("ship")
			test.change(&r)
			if _, err := taskplan.Resolve(r); err == nil {
				t.Fatal("unsupported combination admitted")
			}
		})
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

func TestReconfigurationExtendsPlanWithoutReconstructingUnknownIdentity(t *testing.T) {
	r := request("ship")
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
