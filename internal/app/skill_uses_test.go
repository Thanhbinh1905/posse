package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestClaudeWorkerRoleSkillsAreGuidedRecordedAndShown(t *testing.T) {
	f := newFirstOutcomeFixture(t)
	setupFollowupAgentPath(t, "pi", "claude")
	f.fake.Results["server.agent_manifests"] = json.RawMessage(`{"agents":[{"kind":"pi","available":true},{"kind":"claude","available":true}]}`)
	outcomeCLI(t, f.service, 0, "config", "set", "defaults.merge_mode", "local", "--project", "shop")
	outcomeCLI(t, f.service, 0, "config", "set", "skills.change", `["posse-tdd", "posse-deslop", "posse-no-comments", "posse-unslop"]`)
	outcomeCLI(t, f.service, 0, "config", "set", "profiles.routing.kind", "claude")
	outcomeCLI(t, f.service, 0, "config", "set", "dispatch", "[{type='change', use='routing'}]")
	f.fake.BeforeCall = func(method string) {
		if method == "agent.start" && len(f.fake.SnapshotValue.Panes) == 1 {
			f.fake.SnapshotValue.Panes = append(f.fake.SnapshotValue.Panes, herdr.Pane{PaneID: "fake:child:p1", WorkspaceID: "fake:child", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "working"})
		}
	}
	outcomeCLI(t, f.service, 0, "start", "--brief", f.brief, "--name", "skills")

	launch, err := os.ReadFile(filepath.Join(f.home, "projects", "shop", "tasks", "t1", "launch.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Role skills",
		"- `posse-tdd`: before changing code",
		"- `posse-deslop`: before the `done` Signal",
		"- `posse-no-comments`: before the `done` Signal, after `posse-deslop`",
		"- `posse-unslop`: on PR text, commit messages and Reports, before `posse publish` and the `done` Signal",
	} {
		if !strings.Contains(string(launch), want) {
			t.Fatalf("launch.md lacks %q:\n%s", want, launch)
		}
	}
	if strings.Contains(string(launch), "`posse-better-each-day`:") {
		t.Fatalf("launch.md lists a platform skill as a role skill:\n%s", launch)
	}

	var hooks []byte
	for _, call := range f.fake.Calls {
		if call.Method != "agent.start" {
			continue
		}
		args := call.Params["args"].([]string)
		for index, arg := range args {
			if arg != "--plugin-dir" || index+1 == len(args) {
				continue
			}
			if data, err := os.ReadFile(filepath.Join(args[index+1], "hooks", "hooks.json")); err == nil && bytes.Contains(data, []byte("_skill-hook claude")) {
				hooks = data
			}
		}
	}
	if !bytes.Contains(hooks, []byte(`"PostToolUse"`)) || !bytes.Contains(hooks, []byte(`"Skill"`)) {
		t.Fatalf("Claude Worker launch installs no PostToolUse Skill hook: %s", hooks)
	}

	task, err := f.db.Task(context.Background(), f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerHomeEnv, f.home)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", task.PaneID)
	t.Chdir(task.WorktreePath)
	for _, skill := range []string{"posse-launch-skills:posse-deslop", "posse-launch-skills:posse-deslop", "posse-unslop"} {
		event, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": "worker", "cwd": task.WorktreePath, "tool_name": "Skill", "tool_input": map[string]any{"skill": skill}})
		var stderr bytes.Buffer
		if code := f.service.RunSkillHook(bytes.NewReader(event), &stderr, []string{"claude"}); code != 0 {
			t.Fatalf("skill hook exit=%d: %s", code, &stderr)
		}
	}

	if err := os.WriteFile(filepath.Join(task.WorktreePath, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, task.WorktreePath, "add", "change.txt")
	gitTest(t, task.WorktreePath, "commit", "-m", "change")
	refused := outcomeCLI(t, f.service, 1, "signal", "done", "Changed", "--turn", "launch:1")
	for _, want := range []string{"role_skills_unused", "`posse-tdd`", "`posse-no-comments`"} {
		if !strings.Contains(refused, want) {
			t.Fatalf("refused done lacks %q:\n%s", want, refused)
		}
	}
	if strings.Contains(refused, "`posse-deslop`") || strings.Contains(refused, "`posse-unslop`") {
		t.Fatalf("refused done names a used skill:\n%s", refused)
	}
	for _, skill := range []string{"posse-tdd", "posse-no-comments"} {
		event, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": "worker", "cwd": task.WorktreePath, "tool_name": "Skill", "tool_input": map[string]any{"skill": skill}})
		var stderr bytes.Buffer
		if code := f.service.RunSkillHook(bytes.NewReader(event), &stderr, []string{"claude"}); code != 0 {
			t.Fatalf("skill hook exit=%d: %s", code, &stderr)
		}
	}
	done := outcomeCLI(t, f.service, 0, "signal", "done", "Changed", "--turn", "launch:1")
	for _, want := range []string{"skill_checklist[3]{skill,when,invoked}:", "posse-deslop,\"before the `done` Signal", "posse-no-comments,", "posse-unslop,"} {
		if !strings.Contains(done, want) {
			t.Fatalf("done output lacks %q:\n%s", want, done)
		}
	}
	if strings.Contains(done, "posse-tdd,") {
		t.Fatalf("done checklist lists a skill of another step:\n%s", done)
	}
	for _, row := range strings.Split(done, "\n") {
		if strings.Contains(row, "posse-deslop,") && !strings.HasSuffix(row, ",yes") || strings.Contains(row, "posse-no-comments,") && !strings.HasSuffix(row, ",yes") {
			t.Fatalf("done checklist marks the wrong skills invoked:\n%s", done)
		}
	}

	t.Setenv(workerHomeEnv, "")
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	t.Chdir(f.repo)
	shown := outcomeCLI(t, f.service, 0, "show", "t1")
	for _, want := range []string{"skill_uses_recorded_by: claude hook", "role_skills[4]{skill,uses}:", "posse-tdd,1", "posse-deslop,2", "posse-no-comments,1", "posse-unslop,1"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("show lacks %q:\n%s", want, shown)
		}
	}

	if err := os.Remove(filepath.Join(f.home, "projects", "shop", "tasks", "t1", skillHookMarker)); err != nil {
		t.Fatal(err)
	}
	unhooked := outcomeCLI(t, f.service, 0, "show", "t1")
	if !strings.Contains(unhooked, "skill_uses_recorded_by: \"nothing: Posse records skill uses only for claude Workers launched with the skill hook\"") || strings.Contains(unhooked, "role_skills") {
		t.Fatalf("show of a Task launched without the skill hook reports uses:\n%s", unhooked)
	}
}

func TestRoleSkillChecklistNamesOnlyTheSkillsOfThatStep(t *testing.T) {
	skills := []string{"posse-tdd", "posse-deslop", "posse-technical-writing", "posse-unslop", "go-style"}
	publish := roleSkillChecklist(skills, "publish", map[string]int{"posse-unslop": 1}, true)
	var names []string
	for _, row := range publish {
		names = append(names, row.skill)
	}
	if strings.Join(names, ",") != "posse-technical-writing,posse-unslop" {
		t.Fatalf("publish checklist = %v", names)
	}
	if publish[0].invoked != "no" || publish[1].invoked != "yes" {
		t.Fatalf("publish checklist invoked = %+v", publish)
	}
	if unrecorded := roleSkillChecklist(skills, "done", nil, false); unrecorded[0].invoked != "not recorded" {
		t.Fatalf("unrecorded checklist = %+v", unrecorded)
	}
}
