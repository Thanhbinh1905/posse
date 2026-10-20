//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/config"
)

func TestWorkflowProfileAdmissionAndNoProfileDirectPath(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	baseConfig := `[lead]
kind = "claude"

[defaults]
merge_mode = "pr"
auto_teardown = "never"
pr_poll = "1ms"

[ci_loop]
max_fix_rounds = 0
rerun_cancelled = 0
`
	globalPath := filepath.Join(fixture.home, "config.toml")
	if err := os.WriteFile(globalPath, []byte(baseConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	directBrief := filepath.Join(fixture.root, "direct.md")
	if err := os.WriteFile(directBrief, []byte("---\ntype: change\ntitle: Direct path\ndone_when: checked\n---\nUse one Worker.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", directBrief, "--name", "direct-path"); !strings.Contains(output, "t1") {
		t.Fatalf("start direct Task: %s", output)
	}
	direct, err := fixture.db.Task(context.Background(), fixture.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	directPlan, err := direct.AdmittedPlan()
	if err != nil || directPlan == nil || directPlan.Workflow != nil || directPlan.Launch.ProfileName != "implicit:claude" || directPlan.Launch.Profile.Kind != "claude" || directPlan.Launch.Profile.Model != "" || directPlan.Launch.Profile.Effort != "" || len(directPlan.Launch.Profile.Args) != 0 {
		t.Fatalf("no-profile direct plan changed: %#v %v", directPlan, err)
	}

	globalConfig := baseConfig + `
[workflows.feature]
stages = [
  { name = "Discover", frame = "plan", gate = "user", task_type = "research", workers = "2..4", skills = "<opaque>" },
  { name = "Build", frame = "implement", gate = "user", task_type = "change", workers = "one-per-ticket-category-or-pattern", skills = "<opaque>" },
]
`
	if err := os.WriteFile(globalPath, []byte(globalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	projectConfigPath := filepath.Join(fixture.home, "projects", fixture.project.Name, "config.toml")
	projectConfig := `[workflows.feature]
stages = [
  { name = "Project Plan", frame = "implement", gate = "user", task_type = "change", workers = 1 },
]
`
	if err := os.MkdirAll(filepath.Dir(projectConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfigPath, []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	preview := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "workflow", "show", "feature", "--json")
	var shown struct {
		Name    string                 `json:"name"`
		Source  string                 `json:"source"`
		SHA256  string                 `json:"sha256"`
		Profile config.WorkflowProfile `json:"profile"`
	}
	if err := json.Unmarshal([]byte(preview), &shown); err != nil {
		t.Fatalf("decode workflow preview: %v\n%s", err, preview)
	}
	if shown.Name != "feature" || shown.Source != projectConfigPath || len(shown.Profile.Stages) != 1 || shown.Profile.Stages[0].Name != "Project Plan" {
		t.Fatalf("workflow show did not resolve the whole Project override: %#v", shown)
	}
	digest, err := config.WorkflowDigest(shown.Profile)
	if err != nil || shown.SHA256 != digest {
		t.Fatalf("workflow show digest %q, recomputed %q, err=%v", shown.SHA256, digest, err)
	}

	profileBrief := filepath.Join(fixture.root, "profile.md")
	if err := os.WriteFile(profileBrief, []byte("---\ntype: change\ntitle: Profile path\ndone_when: checked\nworkflow: feature\n---\nUse the selected workflow profile.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", profileBrief, "--name", "profile-path"); !strings.Contains(output, "t2") {
		t.Fatalf("start profiled Task: %s", output)
	}
	profiled, err := fixture.db.Task(context.Background(), fixture.project.ID, "t2")
	if err != nil {
		t.Fatal(err)
	}
	profiledPlan, err := profiled.AdmittedPlan()
	if err != nil || profiledPlan == nil || profiledPlan.Workflow == nil || profiledPlan.Workflow.Name != shown.Name || profiledPlan.Workflow.Source != projectConfigPath || profiledPlan.Workflow.SHA256 != shown.SHA256 || profiledPlan.Workflow.RunID != "w1" || profiledPlan.Workflow.StageName != "Project Plan" || profiledPlan.Workflow.StageIndex != 0 || !reflect.DeepEqual(profiledPlan.Workflow.Profile, shown.Profile) {
		t.Fatalf("Task did not freeze the resolved profile: %#v %v", profiledPlan, err)
	}

	if err := os.WriteFile(projectConfigPath, []byte(strings.ReplaceAll(projectConfig, "Project Plan", "Edited Project Plan")), 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged, err := profiled.AdmittedPlan()
	if err != nil || unchanged == nil || !reflect.DeepEqual(unchanged.Workflow, profiledPlan.Workflow) {
		t.Fatalf("admitted Task followed edited config: %#v %v", unchanged, err)
	}

	invalidGlobal := globalConfig + `
[workflows.invalid]
stages = [{ name = "Broken", frame = "implement", gate = "automatic", task_type = "change", workers = -1 }]
`
	if err := os.WriteFile(globalPath, []byte(invalidGlobal), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidBrief := filepath.Join(fixture.root, "invalid.md")
	if err := os.WriteFile(invalidBrief, []byte("---\ntype: change\ntitle: Invalid config\ndone_when: checked\nworkflow: feature\n---\nThis must not be admitted.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(fixture.binary, "start", "--brief", invalidBrief, "--name", "invalid-config")
	command.Dir, command.Env = fixture.repo, fixture.leadEnv
	output, startErr := command.CombinedOutput()
	if startErr == nil || !strings.Contains(string(output), "config_invalid") || !strings.Contains(string(output), "workflows.invalid.stages[0].gate") || !strings.Contains(string(output), globalPath) {
		t.Fatalf("invalid profile did not fail with file and key: err=%v\n%s", startErr, output)
	}
	tasks, err := fixture.db.Tasks(context.Background(), fixture.project.ID, true)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("invalid config allocated a Task: %#v %v", tasks, err)
	}
	if branches := strings.TrimSpace(gitTest(t, fixture.env, fixture.repo, "for-each-ref", "--format=%(refname)", "refs/heads/posse/t3")); branches != "" {
		t.Fatalf("invalid config allocated a Task branch: %s", branches)
	}
	if _, err := os.Stat(filepath.Join(fixture.home, "projects", fixture.project.Name, "tasks", "t3")); !os.IsNotExist(err) {
		t.Fatalf("invalid config allocated Task output: %v", err)
	}
}
