package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func resolveWorkflowForBrief(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, brief dispatch.Brief) (*config.WorkflowSelection, error) {
	if brief.WorkflowRun == "" {
		if brief.Workflow == "" {
			return nil, nil
		}
		workflow, err := resolveWorkflowSelection(cfg, brief.Workflow)
		if err != nil {
			return nil, err
		}
		stage := workflow.Profile.Stages[0]
		if stage.TaskType == "none" {
			return nil, axi.Failure("workflow_stage_invalid", "a workflow whose first stage is task_type none cannot start from a Task Brief", false, "Start with a Worker stage or choose another profile")
		}
		if stage.TaskType != brief.Type {
			return nil, workflowTaskTypeFailure(workflow.Name, stage.Name, stage.TaskType, brief.Type)
		}
		workflow.RunID, workflow.StageName, workflow.StageIndex = "new", stage.Name, 0
		return workflow, nil
	}

	run, err := db.WorkflowRun(ctx, project.ID, brief.WorkflowRun)
	if errors.Is(err, store.ErrWorkflowRunUnknown) {
		return nil, workflowRunUnknownFailure(brief.WorkflowRun)
	}
	if err != nil {
		return nil, err
	}
	if brief.Workflow != "" && brief.Workflow != run.Profile.Name {
		return nil, axi.Failure("workflow_profile_mismatch", fmt.Sprintf("workflow_run %s uses frozen profile %q, not %q", run.ID, run.Profile.Name, brief.Workflow), false)
	}
	stageIndex := -1
	for index, stage := range run.Profile.Profile.Stages {
		if stage.Name == brief.Stage {
			stageIndex = index
			break
		}
	}
	if stageIndex < 0 {
		return nil, axi.Failure("workflow_stage_unknown", fmt.Sprintf("stage %q is not in frozen workflow run %s", brief.Stage, run.ID), false)
	}
	if run.Status == "complete" {
		return nil, axi.Failure("workflow_run_complete", fmt.Sprintf("workflow run %s is complete", run.ID), false)
	}
	if stageIndex != run.CurrentStageIndex || brief.Stage != run.CurrentStage {
		return nil, axi.Failure("workflow_stage_blocked", fmt.Sprintf("workflow run %s is at stage %q; Task admission for %q requires an approved transition", run.ID, run.CurrentStage, brief.Stage), false,
			"Record the User's approval with `posse workflow advance "+run.ID+" --to "+nextWorkflowStage(run)+" --user-approved \"<User's words>\"` before starting that stage")
	}
	stage := run.Profile.Profile.Stages[stageIndex]
	if stage.TaskType == "none" {
		return nil, axi.Failure("workflow_stage_invalid", fmt.Sprintf("stage %q has task_type none and cannot admit a Worker Task", stage.Name), false)
	}
	if stage.TaskType != brief.Type {
		return nil, workflowTaskTypeFailure(run.Profile.Name, stage.Name, stage.TaskType, brief.Type)
	}
	workflow := run.Profile
	workflow.RunID, workflow.StageName, workflow.StageIndex = run.ID, stage.Name, stageIndex
	return &workflow, nil
}

func nextWorkflowStage(run store.WorkflowRun) string {
	next := run.CurrentStageIndex + 1
	if next < 0 || next >= len(run.Profile.Profile.Stages) {
		return "<no-next-stage>"
	}
	return run.Profile.Profile.Stages[next].Name
}

func workflowTaskTypeFailure(profile, stage, expected, actual string) error {
	return axi.Failure("workflow_task_type_mismatch", fmt.Sprintf("workflow %q stage %q requires Task type %q, not %q", profile, stage, expected, actual), false)
}

func workflowRunUnknownFailure(runID string) error {
	return axi.Failure("workflow_run_unknown", fmt.Sprintf("workflow run %q does not exist in this Project", runID), false, "Start a new run with `workflow: <profile>` or inspect `posse workflow status`")
}

func workflowAdmissionFailure(err error) error {
	switch {
	case errors.Is(err, store.ErrWorkflowRunUnknown):
		return axi.Failure("workflow_run_unknown", err.Error(), false)
	case errors.Is(err, store.ErrWorkflowStageNotCurrent):
		return axi.Failure("workflow_stage_blocked", err.Error(), false)
	case errors.Is(err, store.ErrWorkflowProfileChanged):
		return axi.Failure("workflow_profile_mismatch", err.Error(), false)
	case errors.Is(err, store.ErrWorkflowStageTypeMismatch):
		return axi.Failure("workflow_task_type_mismatch", err.Error(), false)
	case errors.Is(err, store.ErrWorkflowStageNoTask):
		return axi.Failure("workflow_stage_invalid", err.Error(), false)
	case errors.Is(err, store.ErrWorkflowRunComplete):
		return axi.Failure("workflow_run_complete", err.Error(), false)
	default:
		return err
	}
}

func workflowLaunchContext(workflow *config.WorkflowSelection) string {
	if workflow == nil || workflow.RunID == "" || workflow.RunID == "new" || workflow.StageName == "" {
		return ""
	}
	return fmt.Sprintf("\n\n## Workflow stage\n\nThis Task is stage %q (%d/%d) of workflow run `%s` using frozen profile %q (SHA-256 `%s`). Work only on this stage. Completing it does not authorize the next stage; the Lead must record the User's explicit approval. Do not dispatch or start another stage.\n",
		workflow.StageName, workflow.StageIndex+1, len(workflow.Profile.Stages), workflow.RunID, workflow.Name, workflow.SHA256)
}
