package app

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func validationCandidateTargetBranch(ctx context.Context, db *store.DB, project store.Project, task store.Task, candidate store.ValidationCandidate) (string, error) {
	if project.IsWorkspace() && candidate.Repo != "" {
		repos, err := db.TaskRepos(ctx, task.ID)
		if err != nil {
			return "", err
		}
		for _, repo := range repos {
			if repo.Repo == candidate.Repo {
				if repo.TargetBranch == "" {
					return "", axi.Failure("candidate_target_missing", "Member "+repo.Repo+" has no frozen target branch", false)
				}
				return repo.TargetBranch, nil
			}
		}
		return "", axi.Failure("validation_repo_invalid", "candidate Member is not part of this Task", false)
	}
	return taskTargetBranch(project, task), nil
}

func (s *Service) validateCandidate(out *axi.Context, args []string) error {
	parsed, err := parseArgs("validate", args, map[string]flagSpec{
		"repo": {}, "verify": {repeatable: true}, "proof": {}, "no-comments": {}, "no-owner-doc-change": {}, "coverage": {},
	})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || !oneOfString(parsed.Positionals[0], "status", "test", "docs", "lint") {
		return axi.Usage("validate requires status, test, docs or lint")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if !s.workerCaller(out.Context, home) {
		return axi.Failure("worker_only", "validation stages must be recorded by the Change Worker", false)
	}
	task, err := s.workerTask(out.Context, db)
	if err != nil {
		return axi.Failure("worker_task_unknown", "cannot find the Worker's Task", false)
	}
	if !isFullPipelineTask(task, parsed.Flags["repo"]) {
		return axi.Failure("validation_not_selected", "the admitted Task does not select full-pipeline validation", false)
	}
	project, err := db.ProjectByID(out.Context, task.ProjectID)
	if err != nil {
		return err
	}
	if project.IsWorkspace() {
		if parsed.Flags["repo"] == "" {
			return axi.Failure("validation_repo_required", "workspace validation requires --repo <member>", false)
		}
		repos, err := db.TaskRepos(out.Context, task.ID)
		if err != nil {
			return err
		}
		found := false
		for _, repo := range repos {
			if repo.Repo == parsed.Flags["repo"] && repo.State != store.TaskRepoUnchanged {
				found = true
				break
			}
		}
		if !found {
			return axi.Failure("validation_repo_invalid", "the named Member is not changed by this Task", false)
		}
	} else if parsed.Flags["repo"] != "" {
		return axi.Failure("validation_repo_invalid", "--repo is only valid for a workspace Task", false)
	}
	candidate, err := db.LatestValidationCandidate(out.Context, task.ID, parsed.Flags["repo"])
	if err != nil {
		if store.IsNotFound(err) {
			return axi.Failure("candidate_missing", "submit the Task with `posse publish` before validating it", false)
		}
		return err
	}
	stage := parsed.Positionals[0]
	if candidate.State != store.ValidationCandidateActive && candidate.State != store.ValidationCandidatePromoted && !(stage == "status" && candidate.State == store.ValidationCandidateStale) {
		return candidateSubmissionError(task, candidate, "candidate_not_active", "candidate is not available for validation", false)
	}
	if err := s.ensureCandidateCheckout(out.Context, db, home, task, candidate); err != nil {
		return err
	}
	candidate, err = db.LatestValidationCandidate(out.Context, task.ID, parsed.Flags["repo"])
	if err != nil {
		return err
	}
	cwd, err := currentDir()
	if err != nil || !pathWithin(candidate.WorktreePath, cwd) {
		return axi.Failure("validation_custody_required", "run validation from this Task's private candidate checkout", false, candidate.WorktreePath)
	}
	if candidate.State == store.ValidationCandidateActive {
		if err := s.requireCandidateSubmissionFreshness(out.Context, db, task, candidate); err != nil {
			return err
		}
		candidate, err = db.LatestValidationCandidate(out.Context, task.ID, parsed.Flags["repo"])
		if err != nil {
			return err
		}
	}
	if stage == "status" {
		return s.printCandidateValidationStatus(out, db, task, candidate)
	}
	if candidate.State != store.ValidationCandidateActive {
		return axi.Failure("candidate_not_active", "a promoted candidate cannot accept new local validation results", false)
	}
	head, err := gitOutput(out.Context, candidate.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != candidate.CandidateSHA {
		return axi.Failure("candidate_head_unrecorded", "the private candidate changed while validation was starting", true, "Retry `posse validate status`; Posse will bind the new head and invalidate prior results")
	}
	status, err := gitStatusOutput(out.Context, candidate.WorktreePath, "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return axi.Failure("candidate_dirty", "commit candidate changes before recording validation", false)
	}
	targetBranch, err := validationCandidateTargetBranch(out.Context, db, project, task, candidate)
	if err != nil {
		return err
	}
	targetHead, err := remoteBranchHead(out.Context, candidate.WorktreePath, "refs/heads/"+targetBranch)
	if err != nil {
		return axi.Failure("candidate_target_head_failed", "could not inspect the frozen target before validation", true, err.Error())
	}
	if targetHead == "" {
		return axi.Failure("candidate_target_missing", "the frozen target branch is missing from origin", false)
	}
	if targetHead != candidate.TargetSHA {
		if err := db.MarkValidationCandidateStale(out.Context, candidate.ID, targetHead); err != nil {
			return err
		}
		return axi.Failure("candidate_target_moved", "the frozen target moved after candidate submission", false, "Submit a fresh candidate and rerun review -> test -> docs -> lint")
	}
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return admittedPlanFailure(fmt.Errorf("full-pipeline Task has no valid admitted plan"))
	}
	var workflowFailure error
	var workflowCoverage string
	if stage == "test" {
		if err := requireCandidateReviewPass(out.Context, db, project, task, candidate); err != nil {
			return err
		}
		if len(parsed.Repeated["verify"]) == 0 || strings.TrimSpace(parsed.Flags["proof"]) == "" {
			return axi.Failure("validation_evidence_required", "test requires --verify <command -> result> and --proof <intent/E2E evidence>", false)
		}
		for _, verification := range parsed.Repeated["verify"] {
			if command, result, ok := strings.Cut(verification, "->"); !ok || strings.TrimSpace(command) == "" || strings.TrimSpace(result) == "" {
				return axi.Failure("validation_evidence_invalid", "each --verify value must contain a command and result separated by ->", false)
			}
		}
	} else if stage == "docs" {
		if strings.TrimSpace(parsed.Flags["proof"]) == "" || !strings.Contains(strings.ToLower(parsed.Flags["no-comments"]), "pass") {
			return axi.Failure("validation_evidence_required", "docs requires --no-comments passed and --proof <documentation review>", false)
		}
		hasOwnerDocs, err := candidateHasOwnerDocs(out.Context, candidate)
		if err != nil {
			return err
		}
		if !hasOwnerDocs && strings.TrimSpace(parsed.Flags["no-owner-doc-change"]) == "" {
			return axi.Failure("validation_docs_disposition_required", "docs needs an owner-document change or --no-owner-doc-change <reason>", false)
		}
		if !candidateHasCompleteDescription(candidate) {
			return axi.Failure("validation_pr_description_incomplete", "the candidate PR description needs summary, verification, proof, risk and rollback details", false)
		}
	} else if stage == "lint" {
		if _, err := db.ValidationStage(out.Context, candidate.ID, "docs", candidate.CandidateSHA, candidate.TargetSHA, plan.SHA256()); err != nil {
			return axi.Failure("validation_stage_order", "lint follows a passing docs stage on this exact candidate", false)
		}
		if strings.TrimSpace(parsed.Flags["proof"]) == "" || strings.TrimSpace(parsed.Flags["coverage"]) == "" {
			return axi.Failure("validation_evidence_required", "lint requires --proof <result> and --coverage <local workflow jobs and gaps>", false)
		}
		candidateTask := task
		candidateTask.WorktreePath = candidate.WorktreePath
		candidateTask.GatedSHA = candidate.CandidateSHA
		cfg, err := s.prepareProject(out.Context, db, project)
		if err != nil {
			return err
		}
		if gateErr := runGates(out.Context, cfg, candidateTask); gateErr != nil {
			if recordErr := recordGateFailure(out.Context, db, project, task, candidate.CandidateSHA, candidate.Repo, gateErr); recordErr != nil {
				return recordErr
			}
			return axi.Failure("gate_failed", gateErr.Error(), false, "Fix the Gate failure in the private candidate, then rerun review -> test -> docs -> lint")
		}
		scratch, err := ensureTaskScratchForTask(out.Context, db, home, project, task)
		if err != nil {
			return err
		}
		coverage, runErr := runLocalWorkflowJobs(out.Context, candidate.WorktreePath, scratch, plan.WorkflowJobs, parsed.Flags["coverage"])
		workflowCoverage, err = workflowCoverageJSON(coverage)
		if err != nil {
			return err
		}
		workflowFailure = runErr
	}
	if stage == "docs" {
		if _, err := db.ValidationStage(out.Context, candidate.ID, "test", candidate.CandidateSHA, candidate.TargetSHA, plan.SHA256()); err != nil {
			return axi.Failure("validation_stage_order", "docs follows a passing test stage on this exact candidate", false)
		}
	}
	evidence := strings.TrimSpace(parsed.Flags["proof"])
	if stage == "test" {
		evidence = strings.Join(parsed.Repeated["verify"], "\n") + "\n\nIntent/E2E proof:\n" + evidence
	}
	if stage == "docs" {
		evidence = "No-comments check: " + strings.TrimSpace(parsed.Flags["no-comments"]) + "\n" + evidence
		if reason := strings.TrimSpace(parsed.Flags["no-owner-doc-change"]); reason != "" {
			evidence += "\n\nNo owner documentation changes: " + reason
		}
	}
	stageStatus, summary := "passed", stage+" passed"
	coverageText := strings.TrimSpace(parsed.Flags["coverage"])
	if stage == "lint" {
		coverageText = workflowCoverage
	}
	if workflowFailure != nil {
		stageStatus, summary = "failed", "lint blocked by local workflow coverage"
	}
	result, err := db.RecordValidationStage(out.Context, store.ValidationStageResult{
		TaskID: task.ID, CandidateID: candidate.ID, Repo: candidate.Repo, Stage: stage, Status: stageStatus,
		CandidateSHA: candidate.CandidateSHA, TargetSHA: candidate.TargetSHA, ConfigSHA256: plan.SHA256(),
		Summary: summary, Evidence: evidence, Coverage: coverageText,
	})
	if err != nil {
		return err
	}
	if workflowFailure != nil {
		if failedWorkflowJob(coverageText) {
			gateErr := newGateCommandFailure("local workflow job", workflowCoverageFailureOutput(coverageText), workflowFailure)
			if recordErr := recordGateFailure(out.Context, db, project, task, candidate.CandidateSHA, candidate.Repo, gateErr); recordErr != nil {
				return recordErr
			}
			return axi.Failure("gate_failed", gateErr.Error(), false, "Fix the local workflow failure in the private candidate, then rerun review -> test -> docs -> lint")
		}
		return axi.Failure("workflow_jobs_blocked", workflowFailure.Error(), false, "Enable full_pipeline.run_workflows in User Project config before admitting a full-pipeline Task, or resolve the workflow parsing/coverage blocker")
	}
	return out.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)}, {Key: "stage", Value: result.Stage},
		{Key: "candidate_head", Value: result.CandidateSHA}, {Key: "target_head", Value: result.TargetSHA},
		{Key: "config_sha256", Value: result.ConfigSHA256}, {Key: "status", Value: result.Status},
		{Key: "help", Value: []any{"A candidate commit invalidates this result and every later stage; repeat review -> test -> docs -> lint on the new head"}},
	})
}

func requireCandidateReviewPass(ctx context.Context, db *store.DB, project store.Project, task store.Task, candidate store.ValidationCandidate) error {
	targets, err := changeCandidateTargets(ctx, db, project, task)
	if err != nil {
		return err
	}
	_, ok, err := reviewPassFor(ctx, db, task, targets)
	if err != nil {
		return err
	}
	if !ok {
		help := "Run `posse review start " + taskIDString(task.Seq) + "` and wait for a pass on this exact candidate"
		plan, planErr := task.AdmittedPlan()
		if planErr == nil && (plan == nil || plan.ValidationLoops == nil || !plan.ValidationLoops.Review.Enabled) {
			help = "Dispatch an independent Review Task with `review_of: " + taskIDString(task.Seq) + "` and wait for a pass on this exact candidate"
		}
		return axi.Failure("validation_review_required", "the current private candidate "+shortSHA(candidate.CandidateSHA)+" has no independent passing Review", false, help)
	}
	return nil
}

func requireFullPipelineCandidateValidation(ctx context.Context, db *store.DB, project store.Project, task store.Task, candidate store.ValidationCandidate) error {
	if candidate.State != store.ValidationCandidateActive && candidate.State != store.ValidationCandidatePromoted && candidate.State != store.ValidationCandidatePromoting {
		return axi.Failure("candidate_not_active", "candidate is not eligible for full-pipeline promotion", false)
	}
	targetBranch, err := validationCandidateTargetBranch(ctx, db, project, task, candidate)
	if err != nil {
		return err
	}
	targetHead, err := remoteBranchHead(ctx, candidate.WorktreePath, "refs/heads/"+targetBranch)
	if err != nil {
		return err
	}
	if targetHead == "" {
		return axi.Failure("candidate_target_missing", "the frozen target branch is missing from origin", false)
	}
	if targetHead != candidate.TargetSHA {
		if candidate.State == store.ValidationCandidateActive || candidate.State == store.ValidationCandidatePromoting {
			if err := db.MarkValidationCandidateStale(ctx, candidate.ID, targetHead); err != nil {
				return err
			}
		} else if err := db.InvalidateValidationStages(ctx, candidate.ID, targetHead); err != nil {
			return err
		}
		return axi.Failure("validation_stale", "the frozen target moved after local validation", false, "Submit a fresh candidate and rerun review -> test -> docs -> lint")
	}
	return requireFullPipelineCandidateResults(ctx, db, project, task, candidate)
}

func requireFullPipelineCandidateResults(ctx context.Context, db *store.DB, project store.Project, task store.Task, candidate store.ValidationCandidate) error {
	if err := requireCandidateReviewPass(ctx, db, project, task, candidate); err != nil {
		return err
	}
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return admittedPlanFailure(fmt.Errorf("full-pipeline Task has no valid admitted plan"))
	}
	for _, stage := range []string{"test", "docs", "lint"} {
		if _, err := db.ValidationStage(ctx, candidate.ID, stage, candidate.CandidateSHA, candidate.TargetSHA, plan.SHA256()); err != nil {
			return axi.Failure("validation_stage_required", "candidate "+shortSHA(candidate.CandidateSHA)+" has no current passing "+stage+" result", false, "Complete review -> test -> docs -> lint in the private candidate checkout before publishing")
		}
	}
	return nil
}

func requireFullPipelineValidationAt(ctx context.Context, db *store.DB, project store.Project, task store.Task, commit string) error {
	if !project.IsWorkspace() {
		candidate, err := db.LatestValidationCandidate(ctx, task.ID, "")
		if err != nil {
			return axi.Failure("candidate_missing", "full-pipeline Task has no submitted private candidate", false)
		}
		if candidate.State != store.ValidationCandidatePromoted {
			return axi.Failure("candidate_not_promoted", "full-pipeline candidates must be promoted through the private fast-forward path before Merge", false)
		}
		if commit != "" && candidate.CandidateSHA != commit {
			return axi.Failure("validation_stale", "published head does not match the fully validated private candidate", false)
		}
		return requireFullPipelineCandidateValidation(ctx, db, project, task, candidate)
	}
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return admittedPlanFailure(fmt.Errorf("full-pipeline Task has no valid admitted plan"))
	}
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return err
	}
	validated := 0
	for _, repo := range repos {
		if repo.State == store.TaskRepoUnchanged || !isFullPipelineTask(task, repo.Repo) {
			continue
		}
		candidate, err := db.LatestValidationCandidate(ctx, task.ID, repo.Repo)
		if err != nil {
			return axi.Failure("candidate_missing", "full-pipeline Member "+repo.Repo+" has no private candidate", false)
		}
		branchHead, err := gitOutput(ctx, repo.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
		if err != nil {
			return err
		}
		if branchHead != candidate.CandidateSHA || candidate.State != store.ValidationCandidatePromoted {
			return axi.Failure("validation_stale", "Member "+repo.Repo+" head does not match its validated candidate", false)
		}
		if err := requireFullPipelineCandidateValidation(ctx, db, project, task, candidate); err != nil {
			return err
		}
		validated++
	}
	if validated == 0 {
		return axi.Failure("validation_required", "no full-pipeline workspace Members have validated candidates", false)
	}
	return nil
}

func candidateHasOwnerDocs(ctx context.Context, candidate store.ValidationCandidate) (bool, error) {
	changed, err := gitOutput(ctx, candidate.WorktreePath, "diff", "--name-only", candidate.TargetSHA+"..."+candidate.CandidateSHA)
	if err != nil {
		return false, err
	}
	for _, file := range strings.Split(changed, "\n") {
		file = strings.ToLower(strings.TrimSpace(file))
		base := path.Base(file)
		if strings.HasPrefix(file, "docs/") || strings.HasPrefix(base, "readme") || strings.HasPrefix(base, "contributing") || strings.HasSuffix(file, ".md") || strings.HasSuffix(file, ".rst") || strings.HasSuffix(file, ".adoc") {
			return true, nil
		}
	}
	return false, nil
}

func candidateHasCompleteDescription(candidate store.ValidationCandidate) bool {
	return strings.TrimSpace(candidate.Summary) != "" && strings.TrimSpace(candidate.Verification) != "" &&
		strings.TrimSpace(candidate.Proof) != "" && strings.TrimSpace(candidate.Risk) != "" && strings.Contains(strings.ToLower(candidate.Risk), "rollback")
}

func (s *Service) printCandidateValidationStatus(out *axi.Context, db *store.DB, task store.Task, candidate store.ValidationCandidate) error {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return admittedPlanFailure(fmt.Errorf("full-pipeline Task has no valid admitted plan"))
	}
	project, err := db.ProjectByID(out.Context, task.ProjectID)
	if err != nil {
		return err
	}
	targetBranch, err := validationCandidateTargetBranch(out.Context, db, project, task, candidate)
	if err != nil {
		return err
	}
	targetHead, err := remoteBranchHead(out.Context, candidate.WorktreePath, "refs/heads/"+targetBranch)
	if err != nil {
		return err
	}
	if targetHead == "" {
		return axi.Failure("candidate_target_missing", "the frozen target branch is missing from origin", false)
	}
	targetFresh := targetHead == candidate.TargetSHA
	if !targetFresh {
		if err := db.InvalidateValidationStages(out.Context, candidate.ID, targetHead); err != nil {
			return err
		}
	}
	results, err := db.ValidationStages(out.Context, task.ID)
	if err != nil {
		return err
	}
	current := map[string]any{}
	for _, stage := range results {
		if stage.CandidateID == candidate.ID && stage.CandidateSHA == candidate.CandidateSHA && stage.TargetSHA == candidate.TargetSHA && stage.ConfigSHA256 == plan.SHA256() {
			current[stage.Stage] = map[string]any{"status": stage.Status, "evidence": stage.Evidence, "coverage": stage.Coverage}
		}
	}
	reviewPass := false
	if candidate.State == store.ValidationCandidateActive || candidate.State == store.ValidationCandidatePromoted {
		targets, err := changeCandidateTargets(out.Context, db, project, task)
		if err != nil {
			return err
		}
		_, reviewPass, err = reviewPassFor(out.Context, db, task, targets)
		if err != nil {
			return err
		}
	}
	return out.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)}, {Key: "candidate_head", Value: candidate.CandidateSHA},
		{Key: "target_head", Value: candidate.TargetSHA}, {Key: "target_current", Value: targetFresh},
		{Key: "candidate_state", Value: candidate.State}, {Key: "review_pass", Value: reviewPass},
		{Key: "local_stages", Value: current}, {Key: "publication_ready", Value: candidate.State == store.ValidationCandidateActive && targetFresh && reviewPass && current["test"] != nil && current["docs"] != nil && current["lint"] != nil},
	})
}
