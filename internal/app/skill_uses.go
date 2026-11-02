package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	setupassets "github.com/thanhbinh1905/posse/internal/setup"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

// roleSkillStep says when a Worker must invoke a bundled workflow skill and
// which of `publish` and `done` it applies to.
type roleSkillStep struct {
	when  string
	steps []string
}

var proseSkillStep = roleSkillStep{when: "on PR text, commit messages and Reports, before `posse publish` and the `done` Signal", steps: []string{"publish", "done"}}

var roleSkillSteps = map[string]roleSkillStep{
	"posse-tdd":               {when: "before changing code; write the failing test first"},
	"posse-deslop":            {when: "before the `done` Signal, on the lines this branch added or changed", steps: []string{"done"}},
	"posse-no-comments":       {when: "before the `done` Signal, after `posse-deslop`", steps: []string{"done"}},
	"posse-technical-writing": proseSkillStep,
	"posse-unslop":            proseSkillStep,
	"posse-review":            {when: "for the whole review, before the `done` Signal", steps: []string{"done"}},
	"posse-research":          {when: "for the whole investigation, before the `done` Signal", steps: []string{"done"}},
}

// planRoleSkills lists the workflow skills frozen into a plan, without the
// platform skills every session loads.
func planRoleSkills(plan *taskplan.Plan) []string {
	if plan == nil {
		return nil
	}
	platform := setupassets.PlatformSkillNames()
	var names []string
	for _, input := range plan.Inputs {
		if input.Role == "skill" && !slices.Contains(platform, input.Source) {
			names = append(names, input.Source)
		}
	}
	return names
}

func roleSkillWhen(name string) string {
	if step, ok := roleSkillSteps[name]; ok {
		return step.when
	}
	return "when its description matches the work"
}

// roleSkillProtocol tells a Worker when to invoke each loaded role skill.
func roleSkillProtocol(plan *taskplan.Plan) string {
	skills := planRoleSkills(plan)
	if len(skills) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString("\n\n## Role skills\n\nPosse loaded these skills for this role. Invoke each one at its step, not from memory of it:\n")
	for _, name := range skills {
		fmt.Fprintf(&text, "- `%s`: %s.\n", name, roleSkillWhen(name))
	}
	return text.String()
}

type roleSkillCheck struct {
	skill, when, invoked string
}

// roleSkillChecklist lists the skills that apply to step with whether the
// Worker invoked them; recorded is false when the harness reports no uses.
func roleSkillChecklist(skills []string, step string, uses map[string]int, recorded bool) []roleSkillCheck {
	var checks []roleSkillCheck
	for _, name := range skills {
		if !slices.Contains(roleSkillSteps[name].steps, step) {
			continue
		}
		invoked := "not recorded"
		if recorded {
			invoked = "no"
			if uses[name] > 0 {
				invoked = "yes"
			}
		}
		checks = append(checks, roleSkillCheck{skill: name, when: roleSkillWhen(name), invoked: invoked})
	}
	return checks
}

// unusedRoleSkills lists the configured role skills with 0 recorded uses.
// It returns nil when Posse does not record uses for the Task: they are
// unknown there, not zero.
func unusedRoleSkills(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) ([]string, error) {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return nil, err
	}
	if !skillUsesRecorded(plan, skillHookInstalled(home, project.Name, task.Seq)) {
		return nil, nil
	}
	uses, err := db.TaskSkillUses(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	var unused []string
	for _, name := range planRoleSkills(plan) {
		if uses[name] == 0 {
			unused = append(unused, name)
		}
	}
	return unused, nil
}

// unusedRoleSkillsSummary appends the skills with 0 recorded uses to summary.
// A lookup failure leaves the summary unchanged: the Notice must still go out.
func (s *Service) unusedRoleSkillsSummary(ctx context.Context, db *store.DB, project store.Project, task store.Task, summary string) string {
	home, err := s.homePath()
	if err != nil {
		return summary
	}
	unused, err := unusedRoleSkills(ctx, db, home, project, task)
	if err != nil {
		return summary
	}
	return withUnusedRoleSkills(summary, unused)
}

func (s *Service) annotateMergeReadyNotices(ctx context.Context, db *store.DB, project store.Project, task store.Task, notices []store.Notice) {
	for i := range notices {
		if notices[i].Kind == "merge_ready" {
			notices[i].Summary = s.unusedRoleSkillsSummary(ctx, db, project, task, notices[i].Summary)
		}
	}
}

func quotedSkillNames(skills []string) string {
	quoted := make([]string, len(skills))
	for i, name := range skills {
		quoted[i] = "`" + name + "`"
	}
	return strings.Join(quoted, ", ")
}

func roleSkillsUnusedFailure(skills []string) *axi.Error {
	return axi.Failure(
		"role_skills_unused",
		"configured role skills have 0 recorded uses: "+quotedSkillNames(skills),
		false,
		"Invoke each skill at its step, then retry; `posse brief` lists the steps",
	)
}

// requireRoleSkillsUsed refuses while a configured role skill has 0 uses.
func requireRoleSkillsUsed(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task) error {
	unused, err := unusedRoleSkills(ctx, db, home, project, task)
	if err != nil {
		return err
	}
	if len(unused) > 0 {
		return roleSkillsUnusedFailure(unused)
	}
	return nil
}

// withUnusedRoleSkills appends the skills with 0 recorded uses to a Lead
// Notice summary.
func withUnusedRoleSkills(summary string, unused []string) string {
	if len(unused) == 0 {
		return summary
	}
	return summary + "; role skills with 0 recorded uses: " + quotedSkillNames(unused)
}

// skillUsesRecorded reports whether Posse records the skill uses of this
// Task: a claude Worker whose launch installed the Skill hook.
func skillUsesRecorded(plan *taskplan.Plan, hookInstalled bool) bool {
	return plan != nil && plan.Launch.Profile.Kind == "claude" && hookInstalled
}

// taskSkillChecklist is the `skill_checklist` field of publish and done
// output. It returns nil when no loaded role skill applies to step.
func taskSkillChecklist(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task, step string) ([]axi.Field, error) {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return nil, err
	}
	uses, err := db.TaskSkillUses(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	checks := roleSkillChecklist(planRoleSkills(plan), step, uses, skillUsesRecorded(plan, skillHookInstalled(home, project.Name, task.Seq)))
	if len(checks) == 0 {
		return nil, nil
	}
	rows := make([]any, 0, len(checks))
	for _, check := range checks {
		rows = append(rows, axi.Object{{Key: "skill", Value: check.skill}, {Key: "when", Value: check.when}, {Key: "invoked", Value: check.invoked}})
	}
	return []axi.Field{{Key: "skill_checklist", Value: rows}}, nil
}

// taskSkillUseView reports how often the Worker invoked each role skill.
func taskSkillUseView(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task, plan *taskplan.Plan, view map[string]any) error {
	skills := planRoleSkills(plan)
	if len(skills) == 0 {
		return nil
	}
	if !skillUsesRecorded(plan, skillHookInstalled(home, project.Name, task.Seq)) {
		view["skill_uses_recorded_by"] = "nothing: Posse records skill uses only for claude Workers launched with the skill hook"
		return nil
	}
	uses, err := db.TaskSkillUses(ctx, task.ID)
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(skills))
	for _, name := range skills {
		rows = append(rows, map[string]any{"skill": name, "uses": uses[name]})
	}
	view["skill_uses_recorded_by"] = "claude hook"
	view["role_skills"] = rows
	return nil
}

// RunSkillHook is `posse _skill-hook claude`, the Claude Code PostToolUse
// hook of the Skill tool in a Posse-launched Worker. It records the invoked
// skill for the Worker's Task and never blocks the agent.
func (s *Service) RunSkillHook(stdin io.Reader, stderr io.Writer, args []string) int {
	if len(args) != 1 || args[0] != "claude" {
		fmt.Fprintln(stderr, "posse: _skill-hook supports only claude")
		return 1
	}
	data, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		return 0
	}
	var input struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
		AgentID       string `json:"agent_id"`
		ToolInput     struct {
			Skill string `json:"skill"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(data, &input) != nil || input.HookEventName != "PostToolUse" || input.ToolName != "Skill" || input.AgentID != "" {
		return 0
	}
	// Plugin skills arrive as <plugin>:<skill>.
	skill := input.ToolInput.Skill[strings.LastIndex(input.ToolInput.Skill, ":")+1:]
	if skill == "" {
		return 0
	}
	if err := s.recordSkillUse(context.Background(), skill); err != nil {
		fmt.Fprintf(stderr, "posse: could not record skill %s: %v\n", skill, err)
	}
	return 0
}

func (s *Service) recordSkillUse(ctx context.Context, skill string) error {
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	parent, err := s.resolveResearchParent(ctx, db)
	if err != nil {
		return err
	}
	if parent.task.ID == 0 {
		return nil
	}
	return db.RecordTaskSkillUse(ctx, parent.task.ID, skill)
}

// skillHookMarker names the file in a Task's home that records a launch
// installed the Skill hook; Tasks launched before the hook have none.
const skillHookMarker = "skill-hook"

func skillHookInstalled(home, project string, seq int) bool {
	_, err := os.Stat(filepath.Join(home, "projects", project, "tasks", taskIDString(seq), skillHookMarker))
	return err == nil
}

// appendSkillUseLaunchArgs installs the Skill hook plugin for a Claude Worker
// and marks the Task as recorded.
func appendSkillUseLaunchArgs(home string, project store.Project, taskSeq int, kind string, args []string) ([]string, error) {
	if kind != "claude" {
		return args, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate posse executable for the Claude skill hook: %w", err)
	}
	hooks := map[string]any{
		"PostToolUse": []any{map[string]any{"matcher": "Skill", "hooks": []any{map[string]any{"type": "command", "command": shellQuote(executable) + " _skill-hook claude", "timeout": 10}}}},
	}
	root := filepath.Join(home, "projects", project.Name, "claude-skill-uses")
	if err := writeClaudeHookPlugin(root, "posse-skill-uses", "Posse record of the skills a Worker invokes", hooks); err != nil {
		return nil, err
	}
	marker := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(taskSeq), skillHookMarker)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return nil, err
	}
	if err := writeFile(marker, nil); err != nil {
		return nil, err
	}
	return append(args, "--plugin-dir", root), nil
}
