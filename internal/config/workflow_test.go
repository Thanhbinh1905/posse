package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowProfilesUseWholeProjectOverridesAndRecordSource(t *testing.T) {
	home := t.TempDir()
	global := `[workflows.feature]
stages = [
  { name = "Discover", frame = "plan", gate = "user", task_type = "research", workers = "2..4", skills = "<opaque>" },
  { name = "Build", frame = "implement", gate = "user", task_type = "change", workers = "one-per-ticket-category-or-pattern", skills = "<opaque>" },
]

[workflows.bug]
stages = [
  { name = "Diagnose", frame = "plan", gate = "user", task_type = "research", workers = 1, skills = "<opaque>" },
]
`
	project := `[workflows.feature]
stages = [
  { name = "Plan only", frame = "plan", gate = "user", task_type = "research", workers = 1 },
]
`
	globalPath := filepath.Join(home, "config.toml")
	projectPath := filepath.Join(home, "projects", "shop", "config.toml")
	writeConfig(t, globalPath, global)
	writeConfig(t, projectPath, project)

	cfg, err := Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	feature, found, err := cfg.ResolveWorkflow("feature")
	if err != nil || !found {
		t.Fatalf("resolve Project workflow: %#v, found=%t, err=%v", feature, found, err)
	}
	if feature.Source != projectPath || len(feature.Profile.Stages) != 1 || feature.Profile.Stages[0].Name != "Plan only" {
		t.Fatalf("Project profile was not selected as a whole: %#v", feature)
	}
	if digest, err := WorkflowDigest(feature.Profile); err != nil || feature.SHA256 != digest {
		t.Fatalf("profile digest = %q, recomputed %q, err=%v", feature.SHA256, digest, err)
	}
	bug, found, err := cfg.ResolveWorkflow("bug")
	if err != nil || !found || bug.Source != globalPath || len(bug.Profile.Stages) != 1 || bug.Profile.Stages[0].Name != "Diagnose" {
		t.Fatalf("inherited User profile = %#v, found=%t, err=%v", bug, found, err)
	}
	if err := UnsetFile(projectPath, "workflows.feature.stages", true); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	feature, found, err = cfg.ResolveWorkflow("feature")
	if err != nil || !found || feature.Source != globalPath || len(feature.Profile.Stages) != 2 {
		t.Fatalf("unsetting the Project override did not restore the User profile: %#v, found=%t, err=%v", feature, found, err)
	}
}

func TestInvalidWorkflowNamesConfigFileAndKey(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	writeConfig(t, path, `[workflows.broken]
stages = [{ name = "Build", frame = "implement", gate = "automatic", task_type = "change", workers = -1 }]
`)
	_, err := Load(home, "")
	invalid, ok := err.(*InvalidError)
	if !ok || invalid.File != path || invalid.Key != "workflows.broken.stages[0].gate" {
		t.Fatalf("invalid workflow error = %#v, want config file and stage key", err)
	}
}

func TestProjectOverrideDoesNotHideInvalidUserWorkflow(t *testing.T) {
	home := t.TempDir()
	globalPath := filepath.Join(home, "config.toml")
	writeConfig(t, globalPath, `[workflows.feature]
stages = [{ name = "Broken", frame = "plan", gate = "lead", task_type = "research", workers = 1 }]
`)
	writeConfig(t, filepath.Join(home, "projects", "shop", "config.toml"), `[workflows.feature]
stages = [{ name = "Good", frame = "plan", gate = "user", task_type = "research", workers = 1 }]
`)
	_, err := Load(home, "shop")
	invalid, ok := err.(*InvalidError)
	if !ok || invalid.File != globalPath || invalid.Key != "workflows.feature.stages[0].gate" {
		t.Fatalf("shadowed invalid User workflow error = %#v", err)
	}
}

func TestWorkflowStagesCanBeSetThroughSchemaEditor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	_, err := SetFile(path, "workflows.feature.stages", `[{ name = "Build", frame = "implement", gate = "user", task_type = "change", workers = 1, skills = ["opaque"] }]`, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Dir(path), "")
	if err != nil {
		t.Fatal(err)
	}
	workflow, found, err := cfg.ResolveWorkflow("feature")
	if err != nil || !found || len(workflow.Profile.Stages) != 1 || workflow.Profile.Stages[0].Name != "Build" {
		t.Fatalf("edited workflow = %#v, found=%t, err=%v", workflow, found, err)
	}
	if skills, ok := workflow.Profile.Stages[0].Skills.([]any); !ok || len(skills) != 1 || skills[0] != "opaque" {
		t.Fatalf("opaque skills field changed: %#v", workflow.Profile.Stages[0].Skills)
	}
}

func TestWorkflowConfigSchemaMarksStagesUserOnly(t *testing.T) {
	spec, found := Lookup("workflows.feature.stages")
	if !found || spec.Type != TypeTableArray || !IsUserOnly("workflows.feature.stages") {
		t.Fatalf("workflow schema = %#v, found=%t, user-only=%t", spec, found, IsUserOnly("workflows.feature.stages"))
	}
	if !strings.Contains(spec.Meaning, "whole") {
		t.Fatalf("workflow schema does not explain override semantics: %q", spec.Meaning)
	}
}
