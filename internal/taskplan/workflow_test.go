package taskplan_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func TestWorkflowProfileIsFrozenWithSourceAndDigest(t *testing.T) {
	profile := config.WorkflowProfile{Stages: []config.WorkflowStage{{
		Name: "Build", Frame: "implement", Gate: "user", TaskType: "change", Workers: "one-per-ticket-category-or-pattern", Skills: "<opaque>",
	}}}
	digest, err := config.WorkflowDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	workflow := &config.WorkflowSelection{Name: "feature", Profile: profile, Source: "/home/user/.posse/config.toml", SHA256: digest, RunID: "new", StageName: "Build", StageIndex: 0}
	r := request("change")
	r.Brief = taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Frozen workflow\ndone_when: checked\nworkflow: feature\n---\n"))
	r.Workflow = workflow
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Workflow == nil || plan.Workflow.Name != "feature" || plan.Workflow.Source != workflow.Source || plan.Workflow.SHA256 != digest || len(plan.Workflow.Profile.Stages) != 1 {
		t.Fatalf("workflow not admitted: %#v", plan.Workflow)
	}
	workflow.Profile.Stages[0].Name = "Edited after admission"
	if plan.Workflow.Profile.Stages[0].Name != "Build" {
		t.Fatalf("admitted profile aliases mutable config: %#v", plan.Workflow.Profile)
	}
	encoded, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := taskplan.Decode(encoded)
	if err != nil || decoded.Workflow.Profile.Stages[0].Name != "Build" || decoded.Workflow.SHA256 != digest {
		t.Fatalf("workflow did not survive plan round-trip: %#v %v", decoded.Workflow, err)
	}
	var legacy map[string]any
	if err := json.Unmarshal([]byte(encoded), &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["version"] = float64(4)
	legacyWorkflow := legacy["workflow"].(map[string]any)
	delete(legacyWorkflow, "run_id")
	delete(legacyWorkflow, "stage_name")
	delete(legacyWorkflow, "stage_index")
	legacyBytes, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyPlan, err := taskplan.Decode(string(legacyBytes))
	if err != nil || legacyPlan.Version != 4 || legacyPlan.Workflow == nil || legacyPlan.Workflow.RunID != "" {
		t.Fatalf("pre-run workflow plan no longer decodes as historical data: %#v %v", legacyPlan, err)
	}
}

func TestWorkflowDigestPreservesLargeNumericBounds(t *testing.T) {
	profile := config.WorkflowProfile{Stages: []config.WorkflowStage{{
		Name: "Build", Frame: "implement", Gate: "user", TaskType: "change", Workers: int64(9007199254740993),
	}}}
	digest, err := config.WorkflowDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	workflow := &config.WorkflowSelection{Name: "feature", Profile: profile, Source: "user-config.toml", SHA256: digest, RunID: "new", StageName: "Build", StageIndex: 0}
	r := request("change")
	r.Brief = taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Large bound\ndone_when: checked\nworkflow: feature\n---\n"))
	r.Workflow = workflow
	plan, err := taskplan.Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	workers, ok := plan.Workflow.Profile.Stages[0].Workers.(json.Number)
	if !ok || workers.String() != "9007199254740993" || plan.Workflow.SHA256 != digest {
		t.Fatalf("numeric bound lost precision: workers=%#v digest=%s err=%v", plan.Workflow.Profile.Stages[0].Workers, plan.Workflow.SHA256, err)
	}
}

func TestWorkflowMustMatchBriefAndDigest(t *testing.T) {
	profile := config.WorkflowProfile{Stages: []config.WorkflowStage{{Name: "Build", Frame: "implement", Gate: "user", TaskType: "change", Workers: 1}}}
	digest, err := config.WorkflowDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	workflow := &config.WorkflowSelection{Name: "feature", Profile: profile, Source: "user-config.toml", SHA256: digest, RunID: "new", StageName: "Build", StageIndex: 0}
	r := request("change")
	plan, err := taskplan.Resolve(r)
	if err != nil || plan.Workflow != nil {
		t.Fatalf("no-profile direct path changed: %#v %v", plan.Workflow, err)
	}
	r.Workflow = workflow
	if _, err := taskplan.Resolve(r); err == nil || !strings.Contains(err.Error(), "workflow") {
		t.Fatalf("unselected workflow was admitted: %v", err)
	}
	r.Brief = taskplan.Select("brief", "change.md", []byte("---\ntype: change\ntitle: Frozen workflow\ndone_when: checked\nworkflow: feature\n---\n"))
	workflow.SHA256 = strings.Repeat("0", 64)
	if _, err := taskplan.Resolve(r); err == nil || !strings.Contains(err.Error(), "content does not match its digest") {
		t.Fatalf("wrong profile digest was admitted: %v", err)
	}
}
