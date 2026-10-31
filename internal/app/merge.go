package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/execution"
	"github.com/thanhbinh1905/posse/internal/store"
)

func mergeIntentFailure(err error) error {
	if store.IsOnlyBusy(err) {
		return axi.Failure("store_busy", "the store was busy while recording the merge intent", true, "Retry the command after the store is available")
	}
	return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
}

// taskStateChangedFailure reports a normal race with another Task state change
// by public Task ID; the caller can inspect the Task and retry.
func taskStateChangedFailure(task store.Task) error {
	id := taskIDString(task.Seq)
	return axi.Failure("task_state_changed", "Task "+id+" changed state while the command ran", true, "Run `posse show "+id+"` to inspect its state, then retry")
}

// structureTaskStateRace replaces an unstructured store state race, which names
// the database row, with the typed retryable failure for the public Task ID.
func structureTaskStateRace(err error, task store.Task) error {
	var structured *axi.Error
	if err == nil || errors.As(err, &structured) || !isOnlyStateRace(err) {
		return err
	}
	return taskStateChangedFailure(task)
}

func (s *Service) merge(ctx *axi.Context, args []string) (returnErr error) {
	parsed, err := parseArgs("merge", args, map[string]flagSpec{"execute": {boolean: true}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("merge requires one Task id")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	defer func() { returnErr = structureTaskStateRace(returnErr, task) }()
	plan, planErr := task.AdmittedPlan()
	if planErr != nil {
		return admittedPlanFailure(planErr)
	}
	if plan != nil && plan.Execution.Mode == "in-place" {
		return axi.Failure("merge_refused", "in-place Tasks never use Merge", false, "Inspect the recorded source changes with `posse show "+taskIDString(task.Seq)+"`")
	}
	if task.Type != "change" {
		return axi.Failure("merge_refused", "only Change Tasks can be Merged", false)
	}
	if usesRemovedNoMistakes(task) {
		return noMistakesRemoved(task)
	}
	if task.State == store.StateDone && !project.IsWorkspace() {
		work, err := s.unmergedWork(ctx.Context, db, project, task)
		if err != nil {
			return err
		}
		if work == "" {
			return axi.Failure("merge_refused", "Task has no change to deliver", false, "Run `posse teardown "+taskIDString(task.Seq)+"` to finish it")
		}
	}
	if parsed.Bool("execute") {
		if task.State != store.StateDone && task.State != store.StateMerging {
			cfg, err := config.Load(home, project.Name)
			if err != nil {
				return configFailure(err)
			}
			if err := workingPRBehindFailure(ctx.Context, db, project, cfg, task); err != nil {
				return err
			}
			return axi.Failure("merge_refused", "Task must be done or merging before Merge", false)
		}
		if task.AutonomyMerge != "auto" && strings.TrimSpace(parsed.Flags["user-approved"]) == "" {
			mergeKind := "local merge"
			if task.MergeMode == "pr" {
				mergeKind = "pull request merge"
			}
			return axi.Failure("merge_approval_required", mergeKind+" needs User approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
		}
	}
	var prMergeIntent *store.Intent
	if parsed.Bool("execute") && !project.IsWorkspace() && task.MergeMode == "pr" {
		var prIntent store.Intent
		intentErr := retryBusy(ctx.Context, commandContentionAttempts, commandContentionDelay, func() error {
			var err error
			prIntent, err = s.startTaskIntentAtStep(ctx.Context, db, project.ID, task.ID, "merge --execute", "in_progress:pr.merge")
			return err
		})
		if intentErr != nil {
			return mergeIntentFailure(intentErr)
		}
		prMergeIntent = &prIntent
		defer func() {
			if finishErr := db.FinishIntent(ctx.Context, prIntent.ID, prIntent.ProcessID); returnErr == nil && finishErr != nil {
				returnErr = finishErr
			}
		}()
	}
	var cfg config.Config
	err = retryBusy(ctx.Context, commandContentionAttempts, commandContentionDelay, func() error {
		var prepareErr error
		cfg, prepareErr = s.prepareTaskProject(ctx.Context, db, project, task.ID)
		return prepareErr
	})
	if err != nil {
		return err
	}
	task, err = s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.Type != "change" {
		return axi.Failure("merge_refused", "only Change Tasks can be Merged", false)
	}
	if err := pendingCandidateFailure(ctx.Context, db, task); err != nil {
		return err
	}
	if !project.IsWorkspace() && task.MergeMode == "pr" {
		// Pull request evidence binds to the gated forge head, not the Sandbox tip.
		return s.preparePullRequestMerge(ctx, db, project, cfg, task, parsed.Bool("execute"), strings.TrimSpace(parsed.Flags["user-approved"]), prMergeIntent)
	}
	if task.State != store.StateDone || !autoStartReviewLoop(plan) {
		if err := requireSelectedReview(ctx.Context, db, project, cfg, task); err != nil {
			return err
		}
	}
	if project.IsWorkspace() {
		return s.mergeWorkspace(ctx, db, project, cfg, task, parsed.Bool("execute"), strings.TrimSpace(parsed.Flags["user-approved"]))
	}
	var intent store.Intent
	hasIntent := false
	startMergeIntent := func() error {
		if hasIntent {
			return nil
		}
		intentErr := retryBusy(ctx.Context, commandContentionAttempts, commandContentionDelay, func() error {
			var err error
			intent, err = s.startTaskIntent(ctx.Context, db, project.ID, task.ID, "merge --execute")
			return err
		})
		if intentErr == nil {
			hasIntent = true
		}
		return intentErr
	}
	runMergeStep := func(step string, action func() error) error {
		if !hasIntent {
			return action()
		}
		return s.runIntentStep(ctx.Context, db, intent, step, action)
	}
	finishMergeIntent := func() error {
		if !hasIntent {
			return nil
		}
		if err := db.FinishIntent(ctx.Context, intent.ID, intent.ProcessID); err != nil {
			return err
		}
		hasIntent = false
		return nil
	}
	defer func() {
		if !hasIntent {
			return
		}
		if err := settleFailedMergeIntent(ctx.Context, db, project, task, intent); err != nil {
			if returnErr == nil {
				returnErr = err
			}
		}
		hasIntent = false
	}()
	if task.MergeMode != "local" {
		return axi.Failure("merge_refused", "unknown Merge Mode "+task.MergeMode, false)
	}
	if parsed.Bool("execute") {
		if task.State != store.StateDone && task.State != store.StateMerging {
			return axi.Failure("merge_refused", "Task must be done or merging before Merge", false)
		}
		quote := strings.TrimSpace(parsed.Flags["user-approved"])
		if task.AutonomyMerge != "auto" && quote == "" {
			return axi.Failure("merge_approval_required", "local merge needs User approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
		}
	}
	if task.State == store.StateDone {
		if err := verifyFastForward(ctx.Context, project, task); err != nil {
			return err
		}
		gatedSHA, err := gitOutput(ctx.Context, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
		if err != nil {
			return err
		}
		if parsed.Bool("execute") {
			if err := startMergeIntent(); err != nil {
				return mergeIntentFailure(err)
			}
		}
		if err := runMergeStep("gate.record", func() error { return db.SetTaskGatedSHA(ctx.Context, task.ID, gatedSHA) }); err != nil {
			return err
		}
		task.GatedSHA = gatedSHA
		if started, err := s.maybeAutoStartReviewRound(ctx, db, home, project, task); err != nil {
			return err
		} else if started {
			return nil
		}
		if err := requireSelectedReview(ctx.Context, db, project, cfg, task); err != nil {
			return err
		}
		if err := runMergeStep("task.merging", func() error {
			return db.Transition(ctx.Context, task.ID, store.StateDone, store.StateMerging, "cli", "Local fast-forward is ready")
		}); err != nil {
			return err
		}
		if err := runMergeStep("notice.create", func() error {
			_, noticeErr := db.CreateNotice(ctx.Context, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "merge_ready", Summary: s.unusedRoleSkillsSummary(ctx.Context, db, project, task, task.Title+" is ready to merge head "+shortSHA(task.GatedSHA)+" locally"), DataJSON: marshalJSON(map[string]any{"head_sha": task.GatedSHA})})
			return noticeErr
		}); err != nil {
			return err
		}
		if err := s.deliverNotices(ctx.Context, db, project); err != nil {
			return err
		}
		task.State = store.StateMerging
		if !parsed.Bool("execute") {
			if err := s.regenerateProjects(ctx.Context, db); err != nil {
				return err
			}
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "merging"}, {Key: "gated_sha", Value: task.GatedSHA}, {Key: "help", Value: []any{"Run `posse merge " + taskIDString(task.Seq) + " --execute --user-approved \"<User's words>\"` to merge"}}})
		}
	}
	if task.State != store.StateMerging {
		return axi.Failure("merge_refused", "Task must be done or merging before Merge", false, "Run `posse show "+taskIDString(task.Seq)+"` to inspect its state")
	}
	if !parsed.Bool("execute") {
		return axi.Failure("merge_state_invalid", "Task is already waiting for its local merge", false, "Run `posse merge "+taskIDString(task.Seq)+" --execute`")
	}
	if err := startMergeIntent(); err != nil {
		return mergeIntentFailure(err)
	}
	latestSHA, err := gitOutput(ctx.Context, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil {
		return err
	}
	if task.GatedSHA == "" {
		task, err = db.TaskByID(ctx.Context, project.ID, task.ID)
		if err != nil {
			return err
		}
	}
	if task.GatedSHA == "" || latestSHA != task.GatedSHA {
		if err := db.Transition(ctx.Context, task.ID, store.StateMerging, store.StateDone, "cli", "Task branch moved after its merge head was recorded"); err != nil {
			return err
		}
		if err := finishMergeIntent(); err != nil {
			return err
		}
		return axi.Failure("branch_moved", "Task branch moved after its merge head was recorded", false, "Run `posse merge "+taskIDString(task.Seq)+"` again")
	}
	quote := strings.TrimSpace(parsed.Flags["user-approved"])
	if quote != "" {
		if err := runMergeStep("approval.record", func() error { return db.RecordApproval(ctx.Context, task.ID, "merge", quote) }); err != nil {
			return err
		}
	}
	if err := runMergeStep("merge", func() error {
		pathScope, err := localMergePathScope(ctx.Context, project, task)
		if err != nil {
			return err
		}
		targetRef := "refs/heads/" + taskTargetBranch(project, task)
		return withIntentGitResources(ctx.Context, db, project, task, intent.ID, project.Root, []string{targetRef}, pathScope,
			func() error {
				if err := requireSelectedReview(ctx.Context, db, project, cfg, task); err != nil {
					return err
				}
				return mergeLocal(ctx.Context, project, task)
			})
	}); err != nil {
		return err
	}
	mergedRef, err := gitOutput(ctx.Context, project.Root, "rev-parse", "refs/heads/"+taskTargetBranch(project, task))
	if err != nil {
		return err
	}
	if err := runMergeStep("merged_ref.record", func() error { return db.UpdateTaskMerge(ctx.Context, task.ID, task.PRURL, mergedRef) }); err != nil {
		return err
	}
	if err := runMergeStep("task.merged", func() error {
		return db.Transition(ctx.Context, task.ID, store.StateMerging, store.StateMerged, "cli", "Local fast-forward completed at "+mergedRef)
	}); err != nil {
		return err
	}
	if err := finishMergeIntent(); err != nil {
		return err
	}
	if _, err := s.syncProjectRoot(ctx.Context, db, project, cfg, true); err != nil {
		return err
	}
	task.State = store.StateMerged
	if shouldAutoTeardownMerged(cfg) {
		crashIntentAt("merge", "after", "teardown.ready")
		result, completedElsewhere, err := s.teardownMergedTask(ctx.Context, db, project, cfg, task)
		if err != nil {
			return err
		}
		if completedElsewhere {
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "merged"}, {Key: "merged_head", Value: task.GatedSHA}, {Key: "teardown", Value: "torn-down"}, {Key: "help", Value: []any{"The Task Report remains in its Project record"}}})
		}
		return ctx.Print(withTeardownDetails(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "merged"}, {Key: "merged_head", Value: task.GatedSHA}, {Key: "teardown", Value: "torn-down"}, {Key: "closed_panes", Value: result.Panes.Closed}, {Key: "foreign_panes", Value: result.Panes.Foreign}, {Key: "branch_removed", Value: result.BranchRemoved}, {Key: "remote_branch_removed", Value: result.RemoteBranchRemoved}, {Key: "stopped_processes", Value: result.StoppedProcesses}, {Key: "help", Value: []any{"The Task Report remains in its Project record"}}}, result))
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "merged"}, {Key: "merged_head", Value: task.GatedSHA}, {Key: "merged_ref", Value: mergedRef}, {Key: "help", Value: []any{"Run `posse teardown " + taskIDString(task.Seq) + "` to release the Sandbox"}}})
}

// teardownMergedTask reports success only after the Task reaches torn-down.
func (s *Service) teardownMergedTask(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task) (teardownResult, bool, error) {
	current, err := db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return teardownResult{}, false, err
	}
	if current.State == store.StateTornDown {
		return teardownResult{AlreadyTornDown: true}, true, nil
	}
	result, err := s.teardownTask(ctx, db, project, cfg, current, false, "")
	if err == nil {
		return result, result.AlreadyTornDown, nil
	}
	var commandError *axi.Error
	if !errors.As(err, &commandError) || commandError.Code != "intent_active" {
		return result, false, err
	}
	activeError := err
	if err := waitForTaskTeardown(ctx, db, project.ID, task.ID); err != nil {
		return result, false, err
	}
	current, err = db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return result, false, err
	}
	if current.State == store.StateTornDown {
		return teardownResult{AlreadyTornDown: true}, true, nil
	}
	return result, false, activeError
}

func settleFailedMergeIntent(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent) error {
	current, err := db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return err
	}
	if current.State == store.StateMerging {
		if deliveredByPullRequest(current.MergeMode) {
			if err := db.ReconcileResourceLeasesForIntent(ctx, intent.ID, "verified no local merge effect is pending"); err != nil {
				return err
			}
			return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
		}
		merged := false
		if current.GatedSHA != "" {
			_, mergeErr := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", current.GatedSHA, "refs/heads/"+taskTargetBranch(project, current))
			merged = mergeErr == nil
		}
		if merged {
			mergedRef, err := gitOutput(ctx, project.Root, "rev-parse", "refs/heads/"+taskTargetBranch(project, current))
			if err != nil {
				return err
			}
			if err := db.UpdateTaskMerge(ctx, current.ID, current.PRURL, mergedRef); err != nil {
				return err
			}
			if err := db.Transition(ctx, current.ID, store.StateMerging, store.StateMerged, "cli", "Local fast-forward completed at "+mergedRef); err != nil {
				return err
			}
		} else {
			if err := db.Transition(ctx, current.ID, store.StateMerging, store.StateDone, "cli", "Interrupted Merge did not complete its merge"); err != nil {
				return err
			}
			if err := db.ClearTaskGatedSHA(ctx, current.ID); err != nil {
				return err
			}
		}
	}
	if err := db.ReconcileResourceLeasesForIntent(ctx, intent.ID, "re-read the target ref and settled the interrupted local Merge"); err != nil {
		return err
	}
	return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
}

func verifyFastForward(ctx context.Context, project store.Project, task store.Task) error {
	if task.WorktreePath == "" || task.Branch == "" {
		return axi.Failure("merge_refused", "Task has no worktree or branch", false)
	}
	targetRef, err := taskTargetRef(ctx, project, task)
	if err != nil {
		return axi.Failure("target_branch_unavailable", "could not inspect Task target branch "+taskTargetBranch(project, task), true, err.Error())
	}
	// The first commit on an unborn default branch fast-forwards it from nothing.
	unborn := taskTargetBranch(project, task) == project.DefaultBranch && defaultBranchUnborn(ctx, projectRepoTarget(project))
	if _, err := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", targetRef, "refs/heads/"+task.Branch); err != nil && !unborn {
		branch := taskTargetBranch(project, task)
		return axi.Failure("needs_rebase", "Task branch is not a fast-forward of target branch "+branch, false, "Rebase the Task branch on origin/"+branch+" and signal done again")
	}
	status, err := gitStatusOutput(ctx, task.WorktreePath, "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return axi.Failure("merge_refused", "Task worktree is not clean", false, "Commit or discard the remaining changes, then signal done again")
	}
	return nil
}

func mergeLocal(ctx context.Context, project store.Project, task store.Task) error {
	targetBranch := taskTargetBranch(project, task)
	targetRef := "refs/heads/" + targetBranch
	worktrees, err := gitOutput(ctx, project.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	checkedOut := worktreeHasBranch(worktrees, targetBranch)
	if checkedOut {
		branch, err := gitOutput(ctx, project.Root, "branch", "--show-current")
		if err != nil {
			return err
		}
		if branch != targetBranch {
			return axi.Failure("merge_target_busy", "target branch is checked out in a different worktree", false, "Switch the Project checkout to "+targetBranch+" and retry")
		}
		status, err := gitStatusOutput(ctx, project.Root, "--porcelain")
		if err != nil {
			return err
		}
		if status != "" {
			return axi.Failure("merge_target_busy", "Project checkout has uncommitted changes", false, "Clean the Project checkout and retry")
		}
		if _, err := gitOutput(ctx, project.Root, "merge", "--ff-only", task.GatedSHA); err != nil {
			return axi.Failure("needs_rebase", "local fast-forward merge failed", false, err.Error())
		}
		return nil
	}
	if targetBranch != project.DefaultBranch {
		if err := ensureLocalTaskTarget(ctx, project, task); err != nil {
			return axi.Failure("needs_rebase", "could not update target branch "+targetBranch, false, err.Error())
		}
	}
	output, err := gitFetch(ctx, project.Root, ".", task.GatedSHA+":"+targetRef)
	if err != nil {
		details := strings.TrimSpace(output)
		if details == "" {
			details = strings.TrimSpace(err.Error())
		}
		return axi.Failure("needs_rebase", "local fast-forward update failed", false, details)
	}
	return nil
}

func (s *Service) teardown(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("teardown", args, map[string]flagSpec{"discard": {boolean: true}, "keep-branch": {boolean: true}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("teardown requires one Task id")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.State == store.StateTornDown {
		if _, err := s.finishSettledTeardownIntent(ctx.Context, db, project.ID, task.ID); err != nil {
			return err
		}
		return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "torn-down"}, {Key: "already_torn_down", Value: true}, {Key: "help", Value: []any{"Run `posse pool` to inspect released resources"}}})
	}
	if task.State == store.StateQueued {
		if err := db.Transition(ctx.Context, task.ID, store.StateQueued, store.StateTornDown, "cli", "Canceled before Worker dispatch"); err != nil {
			return err
		}
		return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "torn-down"}, {Key: "help", Value: []any{"Queued Task canceled without allocating a Sandbox, pane or branch"}}})
	}
	plan, planErr := task.AdmittedPlan()
	if planErr != nil {
		return admittedPlanFailure(planErr)
	}
	inPlace := plan != nil && plan.Execution.Mode == "in-place"
	quote := parsed.Flags["user-approved"]
	if inPlace && !parsed.Bool("discard") && (task.State == store.StateFailed || task.State == store.StateLost) && strings.TrimSpace(quote) == "" {
		return axi.Failure("decision_required", "failed or lost in-place work requires a User choice to relaunch or preserve", false, "Answer the recovery Decision with `preserve`, or pass `--user-approved \"<User's words>\"` to preserve and stop it")
	}
	if parsed.Bool("discard") {
		if inPlace {
			return axi.Failure("teardown_refused", "in-place source changes cannot be discarded or rolled back", false, "Run `posse teardown <task>` to stop the Worker and preserve its output")
		}
		if strings.TrimSpace(quote) == "" {
			return axi.Failure("teardown_refused", "discard requires a recorded User approval quote", false, "Pass `--discard --user-approved \"<User's words>\"`")
		}
		if !store.IsApprovedDiscardSourceState(task.State) || task.State == store.StateMerged {
			return axi.Failure("teardown_refused", "Task in state "+string(task.State)+" cannot be discarded directly", false)
		}
		cfg, err := config.Load(home, project.Name)
		if err != nil {
			return configError(err)
		}
		result, err := s.teardownTask(ctx.Context, db, project, cfg, task, true, quote, teardownOptions{keepBranch: parsed.Bool("keep-branch")})
		if err != nil {
			return err
		}
		return ctx.Print(withTeardownDetails(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "torn-down"}, {Key: "closed_panes", Value: result.Panes.Closed}, {Key: "foreign_panes", Value: result.Panes.Foreign}, {Key: "branch_removed", Value: result.BranchRemoved}, {Key: "remote_branch_removed", Value: result.RemoteBranchRemoved}, {Key: "stopped_processes", Value: result.StoppedProcesses}, {Key: "help", Value: []any{"Run `posse pool` to inspect released Sandboxes"}}}, result))
	}
	reviewEnded := task.Type == "review" && (task.State == store.StateFailed || task.State == store.StateLost)
	if !inPlace && !reviewEnded && store.IsApprovedDiscardSourceState(task.State) && task.State != store.StateReported && task.State != store.StateMerged {
		work := "a live Worker needs a User-approved cancellation"
		if task.State == store.StateDone {
			var err error
			if work, err = s.unmergedWork(ctx.Context, db, project, task); err != nil {
				return err
			}
		} else if task.State == store.StateFailed {
			empty, err := s.failedStartEmpty(ctx.Context, db, project, task)
			if err != nil {
				return err
			}
			if empty {
				work = ""
			}
		}
		if work != "" {
			return axi.Failure("teardown_refused", "unmerged work requires --discard and User approval", false, work, "Pass `--discard --user-approved \"<User's words>\"`")
		}
	} else if !inPlace && !reviewEnded && task.State != store.StateMerged && task.State != store.StateReported {
		return axi.Failure("teardown_refused", "Task in state "+string(task.State)+" cannot be torn down", false)
	}
	cfg, err := s.prepareTaskProject(ctx.Context, db, project, task.ID)
	if err != nil {
		return err
	}
	preserveQuote := ""
	if inPlace && (task.State == store.StateFailed || task.State == store.StateLost) {
		preserveQuote = quote
	}
	result, err := s.teardownTask(ctx.Context, db, project, cfg, task, false, preserveQuote, teardownOptions{keepBranch: parsed.Bool("keep-branch")})
	if err != nil {
		return err
	}
	return ctx.Print(withTeardownDetails(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "torn-down"}, {Key: "closed_panes", Value: result.Panes.Closed}, {Key: "foreign_panes", Value: result.Panes.Foreign}, {Key: "branch_removed", Value: result.BranchRemoved}, {Key: "remote_branch_removed", Value: result.RemoteBranchRemoved}, {Key: "stopped_processes", Value: result.StoppedProcesses}, {Key: "help", Value: []any{"Run `posse pool` to inspect released resources"}}}, result))
}

type teardownOptions struct {
	keepBranch       bool
	keepPullRequests bool
}

func (s *Service) teardownTask(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, discardable bool, quote string, options ...teardownOptions) (result teardownResult, returnErr error) {
	var option teardownOptions
	if len(options) > 0 {
		option = options[0]
	}
	keepBranch := option.keepBranch
	if discardable {
		if strings.TrimSpace(quote) == "" {
			return result, axi.Failure("teardown_refused", "discard requires a recorded User approval quote", false, "Pass `--discard --user-approved \"<User's words>\"`")
		}
		if !store.IsApprovedDiscardSourceState(task.State) {
			return result, axi.Failure("teardown_refused", "Task in state "+string(task.State)+" cannot be discarded", false)
		}
	}
	release, err := s.holdTaskTeardown(ctx, db, task.ID)
	if err != nil {
		return result, err
	}
	defer release()
	intent, err := s.startTaskIntentWithPayload(ctx, db, project.ID, task.ID, "teardown", marshalJSON(map[string]any{"discard": discardable, "user_quote": quote, "keep_branch": keepBranch, "keep_pull_requests": option.keepPullRequests}))
	if err != nil {
		settled, settleErr := s.finishSettledTeardownIntent(ctx, db, project.ID, task.ID)
		if settleErr != nil {
			return result, settleErr
		}
		if settled {
			result.AlreadyTornDown = true
			return result, nil
		}
		return result, axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	intentFinished := false
	defer func() {
		if intentFinished {
			return
		}
		if finishErr := db.FinishIntent(context.WithoutCancel(ctx), intent.ID, intent.ProcessID); finishErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("finish Teardown intent: %w", finishErr))
		}
	}()
	current, err := db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return result, err
	}
	if current.State == store.StateTornDown {
		result.AlreadyTornDown = true
		return result, nil
	}
	if current.State != task.State {
		return result, fmt.Errorf("%w: Task %d is not in %q", store.ErrStateRace, task.ID, task.State)
	}
	if discardable && (current.Launches != task.Launches || current.AgentName != task.AgentName || current.PaneID != task.PaneID || current.WorktreePath != task.WorktreePath) {
		return result, discardIdentityRefused("Task Worker changed before approved discard acquired its intent")
	}
	task = current
	plan, planErr := task.AdmittedPlan()
	if planErr != nil {
		return result, admittedPlanFailure(planErr)
	}
	inPlace := plan != nil && plan.Execution.Mode == "in-place"
	if discardable && inPlace {
		return result, axi.Failure("teardown_refused", "in-place source changes cannot be discarded or rolled back", false)
	}
	if discardable && !store.IsApprovedDiscardSourceState(task.State) {
		return result, axi.Failure("teardown_refused", "Task in state "+string(task.State)+" cannot be discarded", false)
	}
	sandboxHeld, err := taskSandboxHeldBy(ctx, db, task)
	if err != nil {
		return result, err
	}
	var discardPRs []discardPullRequest
	if discardable {
		discardPRs, err = s.discardPullRequests(ctx, db, project, task)
		if err != nil {
			return result, err
		}
		if err := s.requireDiscardPullRequestsUnmerged(ctx, db, discardPRs, cfg); err != nil {
			return result, err
		}
	}
	home, err := s.homePath()
	if err != nil {
		return result, s.teardownIncomplete(ctx, db, project, task, "Task artifact access", err)
	}
	var processAuthorization *sandboxProcessAuthorization
	if !discardable && s.Herdr != nil && sandboxHeld {
		processAuthorization = newSandboxProcessAuthorization()
		if err := s.verifySandboxForegroundOwnershipWithAuthorization(ctx, db, project, task, processAuthorization); err != nil {
			processAuthorization.Close()
			return result, s.teardownIncomplete(ctx, db, project, task, "Sandbox process ownership", err)
		}
	}
	defer func() {
		if processAuthorization != nil {
			processAuthorization.Close()
		}
	}()
	panePlan, err := s.planTaskPaneTeardown(ctx, project, task)
	if err != nil {
		return result, s.teardownIncomplete(ctx, db, project, task, "pane inspection", err)
	}
	var stopped []string
	var inPlaceShellPID, inPlaceForegroundPID int
	var inPlaceAuthorization *sandboxProcessAuthorization
	var workerPlan *discardWorkerPlan
	if discardable {
		workerPlan, err = s.retainDiscardWorker(ctx, db, project, task, intent, panePlan)
		if err != nil {
			return result, err
		}
		defer workerPlan.Close()
		if !option.keepPullRequests {
			if err := s.closeAndVerifyDiscardPullRequests(ctx, db, discardPRs, cfg); err != nil {
				return result, s.teardownDiscardIncomplete(ctx, db, project, task, "recorded pull request closure", err)
			}
		}
		// The User quote and exact instance bindings are already durable in
		// the intent. Stop before recording the branch tip: interruption can
		// finish a commit, which must be captured and discarded at its final SHA.
		if err := s.runIntentStep(ctx, db, intent, "worker.stop", func() error {
			var stopErr error
			stopped, stopErr = s.stopDiscardWorker(ctx, project, task, workerPlan)
			return stopErr
		}); err != nil {
			return result, s.teardownDiscardIncomplete(ctx, db, project, task, "Worker stop", err)
		}
		// A shutdown handler can emit a final Signal. Accept its new unmerged
		// state, but never reinterpret approval as permission to discard a
		// relaunched Worker or work that has Merged.
		stoppedTask, err := db.TaskByID(ctx, project.ID, task.ID)
		if err != nil {
			return result, err
		}
		if stoppedTask.State == store.StateMerged && task.State != store.StateMerged || !store.IsApprovedDiscardSourceState(stoppedTask.State) || stoppedTask.Launches != task.Launches || stoppedTask.AgentName != task.AgentName || stoppedTask.WorktreePath != task.WorktreePath || stoppedTask.PaneID != task.PaneID || stoppedTask.PRURL != task.PRURL {
			return result, discardIdentityRefused("Task changed launch, PR identity or custody during Worker shutdown")
		}
		task = stoppedTask
		branchSHA := ""
		if project.IsWorkspace() {
			var tipsErr error
			if branchSHA, tipsErr = s.workspaceBranchTips(ctx, db, project, task); tipsErr != nil {
				return result, tipsErr
			}
		} else if task.Branch != "" {
			var shaErr error
			branchSHA, shaErr = gitOutput(ctx, project.Root, "rev-parse", "refs/heads/"+task.Branch)
			if shaErr != nil && !isMissingGitRef(shaErr) {
				return result, shaErr
			}
		}
		if err := s.runIntentStep(ctx, db, intent, "approval.record", func() error {
			_, err := db.RecordDiscardApprovalForIntent(ctx, intent.ID, intent.ProcessID, task.ID, quote, branchSHA)
			return err
		}); err != nil {
			return result, err
		}
	}
	if discardable {
		if err := s.runIntentStep(ctx, db, intent, "discard.capture", func() error {
			return s.captureDiscardTips(ctx, db, home, project, task)
		}); err != nil {
			return result, s.teardownIncomplete(ctx, db, project, task, "discard tip capture", err)
		}
	}
	if !discardable {
		processStopErr := s.runIntentStep(ctx, db, intent, "process.stop", func() error {
			if inPlace {
				paneInfo, authorization, err := s.retainInPlaceTaskProcesses(ctx, db, project, task)
				if err != nil {
					return err
				}
				inPlaceShellPID = paneInfo.ShellPID
				inPlaceForegroundPID = paneInfo.ForegroundProcessGroup
				inPlaceAuthorization = authorization
				if task.AgentName != "" && processIdentityCapabilityError() != "" {
					snapshot, err := s.snapshot(ctx)
					if err != nil {
						return err
					}
					pane, found := findTaskPane(snapshot.Panes, task)
					if found && pane.Agent != "" {
						if err := s.stopPaneAgent(ctx, pane.PaneID, pane.Agent); err != nil {
							return err
						}
					}
				}
			}
			signalGuard := taskProcessSignalGuard(nil)
			if inPlace {
				signalGuard = func(pid int, _ syscall.Signal) error {
					return s.verifyInPlaceProcessSignalOwnership(ctx, project, task, pid, inPlaceShellPID, inPlaceForegroundPID, inPlaceAuthorization)
				}
			} else if processAuthorization != nil {
				signalGuard = processAuthorization.guardSignal
			}
			preservedProcesses, containmentErr := stopTaskProcessContainmentWithGuard(ctx, task, signalGuard, inPlaceShellPID)
			if containmentErr != nil {
				return containmentErr
			}
			excluded := taskProcessDetailsPIDSet(preservedProcesses)
			if sandboxHeld {
				sandboxStopped, stopErr := stopSandboxProcessesAuthorized(task.WorktreePath, processAuthorization, excluded)
				if stopErr != nil {
					return stopErr
				}
				stopped = append(stopped, sandboxStopped...)
			}
			scratchStopped, stopErr := stopTaskScratchProcessesAuthorized(home, project, task, processAuthorization, excluded)
			if stopErr != nil {
				return stopErr
			}
			stopped = append(stopped, scratchStopped...)
			return nil
		})
		if inPlaceAuthorization != nil {
			defer inPlaceAuthorization.Close()
		}
		if processStopErr != nil {
			return result, s.teardownIncomplete(ctx, db, project, task, "Task process shutdown", processStopErr)
		}
	}
	var paneResult teardownPanes
	err = s.runIntentStep(ctx, db, intent, "panes.close", func() error {
		if discardable {
			if err := s.closeDiscardWorkerPanes(ctx, project, task, workerPlan); err != nil {
				return err
			}
		}
		var closeErr error
		paneResult, closeErr = s.verifyTaskPanesClosed(ctx, project, task, panePlan)
		if closeErr == nil {
			s.relabelProjectTabs(ctx, db, project)
		}
		return closeErr
	})
	if err != nil {
		if discardable {
			return result, s.teardownDiscardIncomplete(ctx, db, project, task, "pane closure", err)
		}
		return result, s.teardownIncomplete(ctx, db, project, task, "pane closure", err)
	}
	if err := s.runIntentStep(ctx, db, intent, "sandbox.stop", func() error {
		if discardable {
			if processes := taskContainmentProcessDetails(task); len(processes) > 0 {
				return axi.Failure("discard_processes_present", "refusing approved discard while processes remain in Task-owned paths", true, fmt.Sprint(processes), "No residual process was stopped or signalled. Inspect the Sandbox and scratch processes, then retry discard")
			}
			if err := verifyNoTaskOwnedProcesses(ctx, db, home, project, task); err != nil {
				return err
			}
			return retireEmptyTaskCgroup(task)
		}
		_, excluded, err := taskSharedProcessExclusions(task)
		if err != nil {
			return err
		}
		if !sandboxHeld {
			return nil
		}
		newlyStopped, err := stopSandboxProcessesAuthorized(task.WorktreePath, processAuthorization, excluded)
		if err != nil {
			return err
		}
		stopped = append(stopped, newlyStopped...)
		return nil
	}); err != nil {
		return result, s.teardownIncomplete(ctx, db, project, task, "Task process shutdown", err)
	}
	if sandboxHeld && task.State == store.StateMerged && !discardable && (deliveredByPullRequest(task.MergeMode) || project.IsWorkspace()) {
		if err := s.runIntentStep(ctx, db, intent, "orphan.snapshot", func() error {
			if !project.IsWorkspace() {
				return snapshotPROrphan(ctx, db, project, task)
			}
			members, err := workspaceMembers(ctx, db, project, task)
			if err != nil {
				return err
			}
			requested := map[string]bool{}
			for _, member := range members {
				requested[member.repo.Repo] = true
				if member.repo.State == store.TaskRepoMerged && member.repo.PRURL != "" {
					observation, err := db.LatestMemberPRObservation(ctx, task.ID, member.repo.Repo)
					if err != nil {
						return err
					}
					if err := snapshotPROrphanFromObservation(ctx, db, member.project, member.task, observation, member.repo.Repo); err != nil {
						return err
					}
				} else if err := snapshotUnmergedMemberWork(ctx, db, member.project, member.task, member.repo.Repo); err != nil {
					return err
				}
			}
			// An unrequested Member may still have a detached worktree in a
			// reused Sandbox. Its non-ignored edits belong to this Worker too.
			targets, err := workspaceSandboxTargets(ctx, db, project)
			if err != nil {
				return err
			}
			for _, target := range targets {
				if requested[target.Name] {
					continue
				}
				memberTask := task
				memberTask.WorktreePath = filepath.Join(task.WorktreePath, target.Path)
				if _, err := os.Stat(filepath.Join(memberTask.WorktreePath, ".git")); os.IsNotExist(err) {
					continue
				} else if err != nil {
					return err
				}
				memberProject := project
				memberProject.Root, memberProject.DefaultBranch = target.Root, target.DefaultBranch
				if err := snapshotUnmergedMemberWork(ctx, db, memberProject, memberTask, target.Name); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			_, decisionErr := db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Kind: "orphan", Origin: "orphan:unrecoverable:" + strconv.FormatInt(task.ID, 10), Question: "The merged PR Orphan could not be snapshotted. Repair the Sandbox and retry Teardown, or approve discarding its unsaved work?", Options: []string{"repair", "discard"}})
			if decisionErr != nil {
				return result, errors.Join(err, decisionErr)
			}
			return result, s.teardownIncomplete(ctx, db, project, task, "Orphan snapshot", err)
		}
	}
	if task.State == store.StateReported && !discardable && (sandboxHeld || task.Type == "research") {
		if err := s.runIntentStep(ctx, db, intent, "report.attachments", func() error {
			home, err := s.homePath()
			if err != nil {
				return err
			}
			if sandboxHeld {
				if err := preserveReportAttachments(ctx, db, home, project, task); err != nil {
					return err
				}
			}
			if task.Type == "research" {
				if !sandboxHeld && task.ReportDigest == "" {
					if task.WorktreePath != "" {
						return fmt.Errorf("research Sandbox is not held and no Report bundle has been finalized")
					}
					return nil // Legacy Tasks without a recorded Sandbox cannot be resealed safely.
				}
				return finalizeReportBundle(ctx, db, home, project, task)
			}
			return nil
		}); err != nil {
			_, decisionErr := db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Kind: "orphan", Origin: "orphan:unrecoverable:" + strconv.FormatInt(task.ID, 10), Question: "Report attachments could not be preserved. Repair the Sandbox and retry Teardown, or approve discarding the unsaved attachments?", Options: []string{"repair", "discard"}})
			if decisionErr != nil {
				return result, errors.Join(err, decisionErr)
			}
			return result, s.teardownIncomplete(ctx, db, project, task, "Report attachment preservation", err)
		}
	}
	if err := s.runIntentStep(ctx, db, intent, "scratch.remove", func() error {
		if discardable {
			if err := verifyNoTaskOwnedProcesses(ctx, db, home, project, task); err != nil {
				return err
			}
		} else {
			_, excluded, err := taskSharedProcessExclusions(task)
			if err != nil {
				return err
			}
			newlyStopped, err := stopTaskScratchProcessesAuthorized(home, project, task, processAuthorization, excluded)
			if err != nil {
				return err
			}
			stopped = append(stopped, newlyStopped...)
			scratchProcesses, err := taskScratchProcessDetails(home, project, task)
			if err != nil {
				return err
			}
			for _, process := range scratchProcesses {
				preserved, err := taskProcessHasExcludedAncestor(process.PID, excluded)
				if err != nil {
					return fmt.Errorf("verify preserved daemon process %d before removing Task scratch: %w", process.PID, err)
				}
				if preserved {
					return fmt.Errorf("cannot remove Task scratch while preserved shared daemon process %d (%s) still references it", process.PID, process.Command)
				}
			}
		}
		return removeTaskScratch(home, project, task)
	}); err != nil {
		return result, s.teardownIncomplete(ctx, db, project, task, "Task scratch removal", err)
	}
	var killed []string
	err = s.runIntentStep(ctx, db, intent, "sandbox.release", func() error {
		latestPanes, paneErr := s.verifyTaskPanesClosed(ctx, project, task, panePlan)
		if paneErr != nil {
			return paneErr
		}
		paneResult = latestPanes
		if discardable && sandboxHeld {
			if err := verifyNoTaskOwnedProcesses(ctx, db, home, project, task); err != nil {
				return err
			}
		}
		if !discardable {
			var signalGuard taskProcessSignalGuard
			shellPID := 0
			if inPlace && inPlaceAuthorization == nil && processIdentityCapabilityError() == "" {
				paneInfo, authorization, proofErr := s.retainInPlaceTaskProcesses(ctx, db, project, task)
				if proofErr != nil {
					return proofErr
				}
				inPlaceShellPID = paneInfo.ShellPID
				inPlaceForegroundPID = paneInfo.ForegroundProcessGroup
				inPlaceAuthorization = authorization
				if authorization != nil {
					defer authorization.Close()
				}
			}
			if inPlace {
				shellPID = inPlaceShellPID
				signalGuard = func(pid int, _ syscall.Signal) error {
					return s.verifyInPlaceProcessSignalOwnership(ctx, project, task, pid, inPlaceShellPID, inPlaceForegroundPID, inPlaceAuthorization)
				}
			} else if processAuthorization != nil {
				signalGuard = processAuthorization.guardSignal
			}
			preservedProcesses, err := stopTaskProcessContainmentWithGuard(ctx, task, signalGuard, shellPID)
			if err != nil {
				return err
			}
			if err := retireEmptyTaskCgroupWithPreserved(task, taskProcessDetailsPIDSet(preservedProcesses)); err != nil {
				return err
			}
		}
		if inPlace {
			leases, err := db.ResourceLeases(ctx, project.ID)
			if err != nil {
				return err
			}
			manager := execution.NewManager(db)
			prefix := fmt.Sprintf("in-place:%d:", task.ID)
			for _, lease := range leases {
				if lease.OwnerTaskID == task.ID && strings.HasPrefix(lease.SourceKey, prefix) {
					if err := manager.Reconcile(ctx, lease, "Legacy Directory Task was stopped and its source was preserved"); err != nil {
						return err
					}
				}
			}
		}
		var releaseErr error
		killed, releaseErr = s.releaseSandboxWithAuthorization(ctx, db, project, task, cfg.Pool.Clean, discardable, processAuthorization)
		return releaseErr
	})
	if err != nil {
		return result, s.teardownIncomplete(ctx, db, project, task, "Sandbox release", err)
	}
	result.Panes = paneResult
	result.StoppedProcesses = append(stopped, killed...)
	result.EscapedProcesses = taskOwnedProcessDetails(processAuthorization.escapedProcessPIDs())
	if project.IsWorkspace() {
		err = s.runIntentStep(ctx, db, intent, "branch.remove", func() error {
			if keepBranch {
				return nil
			}
			tips := ""
			if discardable {
				var tipsErr error
				if tips, tipsErr = db.LatestApprovalBranchSHA(ctx, task.ID, "discard"); tipsErr != nil {
					return tipsErr
				}
				if tips == "" {
					return nil
				}
			}
			var removeErr error
			result.BranchRemoved, removeErr = s.removeWorkspaceBranches(ctx, db, project, task, tips)
			return removeErr
		})
		if err != nil {
			return result, s.teardownIncomplete(ctx, db, project, task, "Task branch removal", err)
		}
	} else if discardable {
		if task.Branch != "" {
			err = s.runIntentStep(ctx, db, intent, "branch.remove", func() error {
				if keepBranch {
					return nil
				}
				ref := "refs/heads/" + task.Branch
				sha, err := db.LatestApprovalBranchSHA(ctx, task.ID, "discard")
				if err != nil {
					return err
				}
				if sha != "" {
					if _, err := gitOutput(ctx, project.Root, "update-ref", "-d", ref, sha); err != nil {
						return err
					}
					result.BranchRemoved = true
				}
				return nil
			})
			if err != nil {
				return result, s.teardownIncomplete(ctx, db, project, task, "Task branch removal", err)
			}
		}
	} else {
		if task.Branch != "" {
			err = s.runIntentStep(ctx, db, intent, "branch.remove", func() error {
				if keepBranch || !plan.DeletesBranchOnMerge("") {
					return nil
				}
				// A PR branch advanced after its external merge stays at its
				// existing tip even when the Sandbox can be safely released.
				if deliveredByPullRequest(task.MergeMode) {
					observation, observationErr := db.LatestPRObservation(ctx, task.ID)
					if observationErr != nil && !store.IsNotFound(observationErr) {
						return observationErr
					}
					if observationErr == nil && observation.State == "MERGED" {
						branchSHA, revErr := gitOutput(ctx, project.Root, "rev-parse", "refs/heads/"+task.Branch)
						if revErr == nil && branchSHA != observation.HeadSHA {
							return nil
						}
					}
				}
				ref := "refs/heads/" + task.Branch
				if sha, revErr := gitOutput(ctx, project.Root, "rev-parse", ref); revErr == nil {
					merged := false
					if task.State == store.StateMerged && deliveredByPullRequest(task.MergeMode) {
						observation, observationErr := db.LatestPRObservation(ctx, task.ID)
						if observationErr != nil && !store.IsNotFound(observationErr) {
							return observationErr
						}
						if observationErr == nil && observation.State == "MERGED" {
							if sha != observation.HeadSHA {
								return nil // Preserve work added after the merged PR head.
							}
							merged = true // The recorded merge is enough even if its target ref disappeared.
						}
					}
					if !merged {
						targetRef, targetErr := mergedTaskTargetRef(ctx, project, task)
						if targetErr != nil {
							return targetErr
						}
						if _, mergeErr := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", ref, targetRef); mergeErr == nil {
							merged = true
						}
					}
					if merged {
						if _, err := gitOutput(ctx, project.Root, "update-ref", "-d", ref, sha); err != nil {
							return err
						}
						result.BranchRemoved = true
					}
				}
				return nil
			})
			if err != nil {
				return result, s.teardownIncomplete(ctx, db, project, task, "Task branch removal", err)
			}
		}
	}
	if !discardable && !keepBranch && task.State == store.StateMerged {
		if project.IsWorkspace() {
			members, memberErr := workspaceMembers(ctx, db, project, task)
			if memberErr != nil {
				return result, memberErr
			}
			for _, member := range members {
				if member.repo.State != store.TaskRepoMerged || member.repo.MergeMode != "pr" || member.repo.PRURL == "" {
					continue
				}
				observation, observationErr := db.LatestMemberPRObservation(ctx, task.ID, member.repo.Repo)
				if observationErr != nil {
					return result, observationErr
				}
				removed, cause, removeErr := cleanUpMergedRemoteBranch(ctx, db, member.project, member.task, member.repo.Repo, cfg, observation.HeadSHA)
				if removeErr != nil {
					return result, removeErr
				}
				result.RemoteBranchRemoved = result.RemoteBranchRemoved || removed
				if cause != "" {
					result.RemoteBranchErrors = append(result.RemoteBranchErrors, member.repo.Repo+": "+cause)
				}
			}
		} else if deliveredByPullRequest(task.MergeMode) {
			observation, observationErr := db.LatestPRObservation(ctx, task.ID)
			if observationErr != nil && !store.IsNotFound(observationErr) {
				return result, observationErr
			}
			if observationErr == nil && observation.State == "MERGED" {
				removed, cause, removeErr := cleanUpMergedRemoteBranch(ctx, db, project, task, "", cfg, observation.HeadSHA)
				if removeErr != nil {
					return result, removeErr
				}
				result.RemoteBranchRemoved = removed
				if cause != "" {
					result.RemoteBranchErrors = append(result.RemoteBranchErrors, cause)
				}
			}
		}
	}
	err = s.runIntentStep(ctx, db, intent, "task.torn_down", func() error {
		if discardable {
			return db.TransitionAfterApproval(ctx, task.ID, task.State, store.StateTornDown, "user", "Task Sandbox discarded with User approval", "discard")
		}
		if inPlace && strings.TrimSpace(quote) != "" && (task.State == store.StateFailed || task.State == store.StateLost) {
			return db.TransitionInPlacePreserved(ctx, task, task.State, "in-place source preserved after User chose to stop recovery", quote)
		}
		if task.Type == "review" && (task.State == store.StateFailed || task.State == store.StateLost) {
			return db.TransitionEndedReview(ctx, task.ID, task.State, "Review Task Sandbox released after it ended")
		}
		if !inPlace && task.State == store.StateDone {
			work, workErr := s.unmergedWork(ctx, db, project, task)
			if workErr != nil {
				return workErr
			}
			if work != "" {
				return axi.Failure("teardown_refused", "unmerged work requires --discard and User approval", false, work)
			}
			return db.TransitionWithoutUnmergedWork(ctx, task.ID, task.State, "Task Sandbox released with no unmerged work")
		}
		if !inPlace && task.State == store.StateFailed {
			return db.TransitionNeverStarted(ctx, task.ID, "Task Sandbox released; its Worker never received the Brief")
		}
		note := "Task Sandbox released after completion"
		if inPlace {
			note = "in-place Task stopped after preserving source and releasing path leases"
		}
		return db.Transition(ctx, task.ID, task.State, store.StateTornDown, "cli", note)
	})
	if err != nil {
		return result, s.teardownIncomplete(ctx, db, project, task, "Task state update", err)
	}
	if err := s.regenerateProjects(ctx, db); err != nil {
		return result, err
	}
	// Another Teardown may already have completed this settled intent.
	if err := db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil && !errors.Is(err, store.ErrIntentOwned) {
		return result, err
	}
	intentFinished = true
	if err := recordTeardownEscapedProcesses(ctx, db, project, task, result.EscapedProcesses); err != nil {
		return result, err
	}
	if detail, err := s.trimWorkerGoBuildCache(ctx, home); err != nil {
		result.CacheMaintenance = "warning: " + err.Error()
	} else {
		result.CacheMaintenance = detail
	}
	return result, nil
}

func withTeardownDetails(fields axi.Object, result teardownResult) axi.Object {
	if len(result.EscapedProcesses) > 0 {
		fields = append(fields, axi.Field{Key: "escaped_processes", Value: describeTaskProcesses(result.EscapedProcesses)})
	}
	if result.CacheMaintenance != "" {
		fields = append(fields, axi.Field{Key: "worker_go_cache", Value: result.CacheMaintenance})
	}
	if len(result.RemoteBranchErrors) > 0 {
		fields = append(fields, axi.Field{Key: "remote_branch_errors", Value: result.RemoteBranchErrors})
	}
	return fields
}

func taskSandboxHeldBy(ctx context.Context, db *store.DB, task store.Task) (bool, error) {
	sandbox, err := db.SandboxByTask(ctx, task.ID)
	if store.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sandbox.ID == task.SandboxID && sandbox.TaskID == task.ID && sandbox.State == "held", nil
}

func isMissingGitRef(err error) bool {
	return strings.Contains(err.Error(), "unknown revision") || strings.Contains(err.Error(), "Needed a single revision") || strings.Contains(err.Error(), "not a valid object name")
}
