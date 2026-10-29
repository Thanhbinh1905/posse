package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func resolveWorkflowSelection(cfg config.Config, name string) (*config.WorkflowSelection, error) {
	if name == "" {
		return nil, nil
	}
	workflow, found, err := cfg.ResolveWorkflow(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, unknownWorkflow(name, "")
	}
	return &workflow, nil
}

func frozenLaunch(resolution dispatch.Resolution, kinds map[string]config.Kind) (taskplan.Launch, error) {
	kind, ok := kinds[resolution.Resolved.Kind]
	if !ok {
		return taskplan.Launch{}, axi.Failure("config_invalid", "Task Profile uses unknown agent kind "+resolution.Resolved.Kind, false)
	}
	// Lead-only launch and delivery settings are not inputs to a Worker contract.
	kind = config.Kind{AutoApproveArgs: kind.AutoApproveArgs, ModelArgs: kind.ModelArgs, EffortArgs: kind.EffortArgs, ResumeArgs: kind.ResumeArgs,
		Prepare: kind.Prepare, BackgroundCommands: kind.BackgroundCommands, Steer: kind.Steer, InterruptKeys: kind.InterruptKeys}
	args := workerAgentArgs(resolution.Resolved, kind, "")
	if resolution.Resolved.Kind == "codex" {
		if _, err := mergeCodexDeveloperInstructionsWithBase(args, "", ""); err != nil {
			return taskplan.Launch{}, err
		}
	}
	identity := configuredLaunchIdentity(resolution.Profile, resolution.Resolved, kind, args)
	return taskplan.Launch{ProfileName: resolution.Profile, DispatchRule: resolution.Rule, Profile: resolution.Resolved, Kind: kind, Args: args,
		ConfiguredModel: identity.ConfiguredModel, ModelKnown: identity.ModelKnown}, nil
}

func resolveTaskPlan(ctx context.Context, home string, project store.Project, cfg config.Config, task store.Task, resolution dispatch.Resolution, briefPath string, briefData []byte, members []taskMember, choice *taskplan.UserChoice, workflow *config.WorkflowSelection) (taskplan.Plan, error) {
	brief, err := dispatch.ParseBriefText(string(briefData))
	if err != nil {
		return taskplan.Plan{}, err
	}
	if project.IsDirectory() {
		return taskplan.Plan{}, axi.Failure("project_directory_removed", "Directory Projects no longer admit Tasks", false)
	}
	launch, err := frozenLaunch(resolution, cfg.Kinds)
	if err != nil {
		return taskplan.Plan{}, err
	}
	execution := taskplan.Execution{Mode: "sandbox", Scope: project.Kind, Base: task.BaseRef, Target: task.TargetBranch, RootContext: brief.RootContext}
	if !project.IsWorkspace() && execution.Target == "" {
		execution.Target = project.DefaultBranch
	}
	if execution.Scope == "" {
		execution.Scope = "repo"
	}
	if brief.Base != "" && !project.IsWorkspace() {
		execution.BaseCommit, err = selectedBaseCommit(ctx, project.Root, task.BaseRef)
		if err != nil {
			return taskplan.Plan{}, err
		}
	}
	for _, member := range members {
		selected := taskplan.Member{Name: member.Name, Path: member.Path, Base: member.BaseRef, Target: defaultValue(member.TargetBranch, member.DefaultBranch), Delivery: member.MergeMode, Review: cfg.MemberReview(member.Name), ReviewSource: "defaults.review"}
		deleteBranch := cfg.MemberDeleteBranchOnMerge(member.Name)
		selected.DeleteBranchOnMerge = &deleteBranch
		if cfg.Repositories[member.Name].Review != "" {
			selected.ReviewSource = "repositories." + member.Name + ".review"
		}
		if _, explicit := branchSelection(brief.Base, brief.BaseByRepo, member.Name, ""); explicit {
			selected.BaseCommit, err = selectedBaseCommit(ctx, member.Root, member.BaseRef)
			if err != nil {
				return taskplan.Plan{}, err
			}
		}
		execution.Members = append(execution.Members, selected)
	}
	preferences, err := preferenceSources(home, project, "worker")
	if err != nil {
		return taskplan.Plan{}, err
	}
	inputs := []taskplan.Input{selectedLaunchSettings(launch)}
	for _, source := range preferences {
		if !source.Exists {
			continue
		}
		input := taskplan.Select("preferences", source.Path, []byte(source.Content))
		input.Layer, input.Legacy = source.Layer, source.Legacy
		inputs = append(inputs, input)
	}
	knowledge, err := knowledgePlanInputs(home, project, cfg)
	if err != nil {
		return taskplan.Plan{}, err
	}
	inputs = append(inputs, knowledge...)
	skills, err := selectedSkillInputs(cfg, task.Type, launch.Profile.Kind)
	if err != nil {
		return taskplan.Plan{}, err
	}
	inputs = append(inputs, skills...)
	root, err := filepath.Abs(home)
	if err != nil {
		return taskplan.Plan{}, err
	}
	// The owning home already exists. Resolve it just as Task scratch does.
	root = resolvedPath(root)
	source, err := filepath.Abs(briefPath)
	if err != nil {
		return taskplan.Plan{}, err
	}
	delivery := projectMergeMode(ctx, project, cfg)
	review := cfg.Defaults.Review
	if project.IsWorkspace() {
		delivery = task.MergeMode
	}
	return taskplan.Resolve(taskplan.Request{Brief: taskplan.Select("brief", source, briefData), Inputs: inputs, Execution: execution, Launch: launch,
		Review: review, ReviewOf: task.ReviewsTaskID, Delivery: delivery,
		Autonomy: cfg.Autonomy, ConfigSources: cfg.Files, UserChoice: choice, ProseLanguage: artifactProseLanguage,
		Paths:           taskplan.Paths{Scratch: filepath.Join(root, "scratch", project.Name, "{task}"), Output: filepath.Join(root, "projects", project.Name, "tasks", "{task}")},
		WorkflowJobs:    cfg.FullPipeline.RunWorkflows,
		ValidationLoops: &taskplan.ValidationLoops{Review: cfg.ReviewLoop, CI: cfg.CILoop},
		MergeMethod:     cfg.Defaults.MergeMethod, Workflow: workflow, Pool: config.Pool{Clean: cfg.Pool.Clean, Setup: cfg.Pool.Setup},
		DeleteBranchOnMerge: cfg.Defaults.DeleteBranchOnMerge})
}

// Restore files absent after a crash from the admitted inputs, never from
// mutable Brief, preferences or Profile sources. Existing launch text is kept.
func restoreAdmittedLaunchFiles(home string, project store.Project, task store.Task, plan *taskplan.Plan) error {
	roundFiles := make(map[string][]byte)
	for _, input := range plan.Inputs {
		if input.Role == "review-round-file" {
			roundFiles[input.Source] = input.Bytes
		}
	}
	if len(roundFiles) > 0 {
		if err := writeReviewRoundFiles(plan.Paths.Output, roundFiles); err != nil {
			return err
		}
	}
	briefPath := filepath.Join(plan.Paths.Output, "brief.md")
	if _, err := os.Stat(briefPath); os.IsNotExist(err) {
		briefBytes := plan.Inputs[0].Bytes
		if len(roundFiles) > 0 {
			briefBytes = []byte(withReviewDir(string(briefBytes), plan.Paths.Output))
		}
		if err := writeFile(briefPath, briefBytes); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	launchPath := filepath.Join(plan.Paths.Output, "launch.md")
	if _, err := os.Stat(launchPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	briefText := string(plan.Inputs[0].Bytes)
	if len(roundFiles) > 0 {
		briefText = withReviewDir(briefText, plan.Paths.Output)
	}
	brief, err := dispatch.ParseHistoricalBriefText(briefText)
	if err != nil {
		return err
	}
	var members []taskMember
	for _, member := range plan.Execution.Members {
		members = append(members, taskMember{repoTarget: repoTarget{Name: member.Name, DefaultBranch: member.Target}, Path: member.Path, BaseRef: member.Base, TargetBranch: member.Target, MergeMode: member.Delivery})
	}
	contents := workerProtocol(home, project, task, brief, launchPath, plan.Paths.Scratch, plan.Execution.RootContext) + workflowLaunchContext(plan.Workflow) + workspaceProtocol(project, members, plan.Execution.RootContext) + workerWaitRules(plan.Launch.Kind) + "\n\n" + brief.Body + "\n"
	contents = appendPreferenceInstructions(contents, "worker", planPreferences(plan))
	contents = appendKnowledgeInstructions(contents, project, planKnowledge(plan))
	return writeFile(launchPath, []byte(contents))
}

func planPreferences(plan *taskplan.Plan) []preferenceSource {
	var sources []preferenceSource
	for _, input := range plan.Inputs {
		if input.Role == "preferences" {
			sources = append(sources, preferenceSource{Layer: input.Layer, Path: input.Source, Content: string(input.Bytes), Exists: true, Legacy: input.Legacy})
		}
	}
	return sources
}

// status is deliberately a read-only database view. It does not open the
// migrating store, prepare a Project, inspect Herdr or poll a forge.
func (s *Service) status(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("status takes no Task id; a Worker may inspect only its own status")
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	db, err := store.OpenReadOnly(home)
	if err != nil {
		return err
	}
	defer db.Close()
	task, err := s.workerTask(ctx.Context, db)
	if err != nil {
		if store.IsBusy(err) {
			return err
		}
		return axi.Failure("worker_task_unknown", "cannot find this Worker's Task", false, "Run `posse status` from the Worker's pane or Sandbox")
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	admission := "legacy"
	if plan != nil {
		admission = "admitted"
	}
	output := axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: string(task.State)}, {Key: "admission", Value: admission}, {Key: "launches", Value: task.Launches}, {Key: "review_evidence", Value: reviewEvidenceStatus(task)}}
	if children, err := taskResearchViews(ctx.Context, db, task); err != nil {
		return err
	} else if len(children) > 0 {
		output = append(output, axi.Field{Key: "research_children", Value: children})
	}
	return ctx.Print(append(output, axi.Field{Key: "plan", Value: plan}))
}

func admittedPlanFailure(err error) error {
	if _, ok := err.(*dispatch.BriefError); ok {
		return axi.Failure("brief_invalid", err.Error(), false)
	}
	return axi.Failure("plan_invalid", fmt.Sprintf("Task execution plan refused: %v", err), false)
}

func selectedSkillInputs(cfg config.Config, taskType, kind string) ([]taskplan.Input, error) {
	var inputs []taskplan.Input
	if kind == "codex" {
		instructions, err := codexConfigDeveloperInstructions()
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, taskplan.Select("codex-developer-instructions", "Codex config: developer_instructions", []byte(instructions)))
	}
	if kind == "claude" || kind == "codex" || kind == "pi" {
		skills, err := roleSkillInputs(cfg, taskType)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, skills...)
	}
	return inputs, nil
}

// selectReconfiguredLaunchInputs keeps the skills frozen at admission; a plan
// that had none, because its old kind loads no skills, selects them now.
func selectReconfiguredLaunchInputs(plan taskplan.Plan, cfg config.Config) (taskplan.Plan, error) {
	inputs := []taskplan.Input{}
	var frozenSkills []taskplan.Input
	for _, input := range plan.Inputs {
		switch input.Role {
		case "skill", "skill-asset":
			frozenSkills = append(frozenSkills, input)
		case "launch-settings", "codex-developer-instructions":
		default:
			inputs = append(inputs, input)
		}
	}
	inputs = append(inputs, selectedLaunchSettings(plan.Launch))
	selected, err := selectedSkillInputs(cfg, plan.Type, plan.Launch.Profile.Kind)
	if err != nil {
		return taskplan.Plan{}, err
	}
	loadsSkills := false
	for _, input := range selected {
		if input.Role == "skill" || input.Role == "skill-asset" {
			loadsSkills = true
			if len(frozenSkills) > 0 {
				continue
			}
		}
		inputs = append(inputs, input)
	}
	if loadsSkills {
		inputs = append(inputs, frozenSkills...)
	}
	plan.Inputs = inputs
	return plan, plan.Validate()
}

func appendAdmittedSkillArgs(kind string, args []string, skills posseLaunchSkills, plan *taskplan.Plan) ([]string, error) {
	if plan != nil && kind == "codex" {
		for _, input := range plan.Inputs {
			if input.Role == "codex-developer-instructions" {
				return mergeCodexDeveloperInstructionsWithBase(args, skills.CodexInstructions, string(input.Bytes))
			}
		}
		return nil, fmt.Errorf("admitted plan has no selected Codex developer instructions")
	}
	return appendPosseSkillLaunchArgs(kind, args, skills)
}

// selectedLaunchSettings snapshots normalized fields consumed by the launch,
// never unrelated Profiles, credentials or forge comments.
func selectedLaunchSettings(launch taskplan.Launch) taskplan.Input {
	data, _ := json.Marshal(launch)
	return taskplan.Select("launch-settings", "effective launch configuration", data)
}
