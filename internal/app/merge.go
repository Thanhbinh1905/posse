package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

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
	if parsed.Bool("execute") {
		if task.State != store.StateDone && task.State != store.StateMerging {
			return axi.Failure("merge_refused", "Task must be done or merging before Merge", false)
		}
		if task.AutonomyMerge != "auto" && strings.TrimSpace(parsed.Flags["user-approved"]) == "" {
			mergeKind := "local merge"
			if task.MergeMode == "pr" || task.MergeMode == "no-mistakes" {
				mergeKind = "pull request merge"
			}
			return axi.Failure("merge_approval_required", mergeKind+" needs User approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
		}
	}
	cfg, err := s.prepareProject(ctx.Context, db, project)
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
	if !project.IsWorkspace() && (task.MergeMode == "pr" || task.MergeMode == "no-mistakes") {
		// Pull request evidence binds to the gated forge head, not the Sandbox tip.
		return s.preparePullRequestMerge(ctx, db, project, cfg, task, parsed.Bool("execute"), strings.TrimSpace(parsed.Flags["user-approved"]))
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
		var intentErr error
		intent, intentErr = s.startTaskIntent(ctx.Context, db, project.ID, task.ID, "merge --execute")
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
				return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
			}
		}
		if err := runMergeStep("gate.record", func() error { return db.SetTaskGatedSHA(ctx.Context, task.ID, gatedSHA) }); err != nil {
			return err
		}
		task.GatedSHA = gatedSHA
		if err := runMergeStep("gate.run", func() error { return runGates(ctx.Context, cfg, task) }); err != nil {
			_, noticeErr := db.CreateNotice(ctx.Context, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "gate_failed", Summary: task.Title + ": " + truncate(err.Error(), 240), DataJSON: `{}`})
			if noticeErr != nil {
				return noticeErr
			}
			if finishErr := finishMergeIntent(); finishErr != nil {
				return finishErr
			}
			_ = s.deliverNotices(ctx.Context, db, project)
			return axi.Failure("gate_failed", err.Error(), false, "Fix the gate failure on the Task branch, then run `posse send "+taskIDString(task.Seq)+" <instruction>`")
		}
		if started, err := s.maybeAutoStartReviewRound(ctx, db, home, project, task); err != nil {
			return err
		} else if started {
			return nil
		}
		if err := requireSelectedReview(ctx.Context, db, project, cfg, task); err != nil {
			return err
		}
		if err := runMergeStep("task.merging", func() error {
			return db.Transition(ctx.Context, task.ID, store.StateDone, store.StateMerging, "cli", "Gate passed and local fast-forward is ready")
		}); err != nil {
			return err
		}
		if err := runMergeStep("notice.create", func() error {
			_, noticeErr := db.CreateNotice(ctx.Context, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "merge_ready", Summary: task.Title + " is ready to merge locally", DataJSON: `{}`})
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
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "merging"}, {Key: "help", Value: []any{"Run `posse merge " + taskIDString(task.Seq) + " --execute --user-approved \"<User's words>\"` to merge"}}})
		}
	}
	if task.State != store.StateMerging {
		return axi.Failure("merge_refused", "Task must be done or merging before Merge", false, "Run `posse show "+taskIDString(task.Seq)+"` to inspect its state")
	}
	if !parsed.Bool("execute") {
		return axi.Failure("merge_state_invalid", "Task is already waiting for its local merge", false, "Run `posse merge "+taskIDString(task.Seq)+" --execute`")
	}
	if err := startMergeIntent(); err != nil {
		return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
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
		if err := db.Transition(ctx.Context, task.ID, store.StateMerging, store.StateDone, "cli", "Task branch moved after the Gate passed"); err != nil {
			return err
		}
		if err := finishMergeIntent(); err != nil {
			return err
		}
		return axi.Failure("branch_moved", "Task branch moved after the Gate passed", false, "Run `posse merge "+taskIDString(task.Seq)+"` again to re-gate")
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
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "merged"}, {Key: "teardown", Value: "torn-down"}, {Key: "help", Value: []any{"The Task Report remains in its Project record"}}})
		}
		return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "merged"}, {Key: "teardown", Value: "torn-down"}, {Key: "closed_panes", Value: result.Panes.Closed}, {Key: "foreign_panes", Value: result.Panes.Foreign}, {Key: "branch_removed", Value: result.BranchRemoved}, {Key: "stopped_processes", Value: result.StoppedProcesses}, {Key: "help", Value: []any{"The Task Report remains in its Project record"}}})
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "merged"}, {Key: "merged_ref", Value: mergedRef}, {Key: "help", Value: []any{"Run `posse teardown " + taskIDString(task.Seq) + "` to release the Sandbox"}}})
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
	if err := waitForActiveTeardowns(ctx, db, project.ID); err != nil {
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
		if current.MergeMode == "pr" || current.MergeMode == "no-mistakes" {
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

func runGates(ctx context.Context, cfg config.Config, task store.Task) error {
	for _, gate := range cfg.Defaults.Gate {
		output, err := commandOutput(ctx, task.WorktreePath, gate)
		if err != nil {
			return fmt.Errorf("gate %q failed: %s", gate, truncate(strings.TrimSpace(output), 1200))
		}
	}
	return nil
}

func verifyFastForward(ctx context.Context, project store.Project, task store.Task) error {
	if task.WorktreePath == "" || task.Branch == "" {
		return axi.Failure("merge_refused", "Task has no worktree or branch", false)
	}
	targetRef, err := taskTargetRef(ctx, project, task)
	if err != nil {
		return axi.Failure("target_branch_unavailable", "could not inspect Task target branch "+taskTargetBranch(project, task), true, err.Error())
	}
	if _, err := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", targetRef, "refs/heads/"+task.Branch); err != nil {
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
	parsed, err := parseArgs("teardown", args, map[string]flagSpec{"discard": {boolean: true}, "user-approved": {}})
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
		return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "torn-down"}, {Key: "already_torn_down", Value: true}, {Key: "help", Value: []any{"Run `posse pool` to inspect released resources"}}})
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
		result, err := s.teardownTask(ctx.Context, db, project, cfg, task, true, quote)
		if err != nil {
			return err
		}
		return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "torn-down"}, {Key: "closed_panes", Value: result.Panes.Closed}, {Key: "foreign_panes", Value: result.Panes.Foreign}, {Key: "branch_removed", Value: result.BranchRemoved}, {Key: "stopped_processes", Value: result.StoppedProcesses}, {Key: "help", Value: []any{"Run `posse pool` to inspect released Sandboxes"}}})
	}
	if !inPlace && store.IsApprovedDiscardSourceState(task.State) && task.State != store.StateReported && task.State != store.StateMerged {
		return axi.Failure("teardown_refused", "unmerged work requires --discard and User approval", false, "Pass `--discard --user-approved \"<User's words>\"`")
	}
	if !inPlace && task.State != store.StateMerged && task.State != store.StateReported {
		return axi.Failure("teardown_refused", "Task in state "+string(task.State)+" cannot be torn down", false)
	}
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	preserveQuote := ""
	if inPlace && (task.State == store.StateFailed || task.State == store.StateLost) {
		preserveQuote = quote
	}
	result, err := s.teardownTask(ctx.Context, db, project, cfg, task, false, preserveQuote)
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "torn-down"}, {Key: "closed_panes", Value: result.Panes.Closed}, {Key: "foreign_panes", Value: result.Panes.Foreign}, {Key: "branch_removed", Value: result.BranchRemoved}, {Key: "stopped_processes", Value: result.StoppedProcesses}, {Key: "help", Value: []any{"Run `posse pool` to inspect released resources"}}})
}

func (s *Service) teardownTask(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, discardable bool, quote string) (teardownResult, error) {
	result := teardownResult{}
	if discardable {
		if strings.TrimSpace(quote) == "" {
			return result, axi.Failure("teardown_refused", "discard requires a recorded User approval quote", false, "Pass `--discard --user-approved \"<User's words>\"`")
		}
		if !store.IsApprovedDiscardSourceState(task.State) {
			return result, axi.Failure("teardown_refused", "Task in state "+string(task.State)+" cannot be discarded", false)
		}
	}
	intent, err := s.startTaskIntentWithPayload(ctx, db, project.ID, task.ID, "teardown", marshalJSON(map[string]any{"discard": discardable, "user_quote": quote}))
	if err != nil {
		return result, axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	defer func() { _ = db.FinishIntent(ctx, intent.ID, intent.ProcessID) }()
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
	if !discardable && s.Herdr != nil && task.AgentName != "" && sandboxHeld {
		processAuthorization = newSandboxProcessAuthorization()
		if err := s.verifySandboxForegroundOwnerchangeWithAuthorization(ctx, db, project, task, processAuthorization); err != nil {
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
	var workerPlan *discardWorkerPlan
	if discardable {
		workerPlan, err = s.retainDiscardWorker(ctx, db, project, task, intent, panePlan)
		if err != nil {
			return result, err
		}
		defer workerPlan.Close()
		if err := s.closeAndVerifyDiscardPullRequests(ctx, db, discardPRs, cfg); err != nil {
			return result, s.teardownDiscardIncomplete(ctx, db, project, task, "recorded pull request closure", err)
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
		if err := s.runIntentStep(ctx, db, intent, "process.stop", func() error {
			if inPlace && task.AgentName != "" && s.Herdr != nil {
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
			if sandboxHeld {
				sandboxStopped, stopErr := stopSandboxProcessesAuthorized(task.WorktreePath, processAuthorization)
				if stopErr != nil {
					return stopErr
				}
				stopped = append(stopped, sandboxStopped...)
			}
			scratchStopped, stopErr := stopTaskScratchProcessesAuthorized(home, project, task, processAuthorization)
			if stopErr != nil {
				return stopErr
			}
			stopped = append(stopped, scratchStopped...)
			return nil
		}); err != nil {
			return result, s.teardownIncomplete(ctx, db, project, task, "Task process shutdown", err)
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
			return verifyNoTaskOwnedProcesses(ctx, db, home, project, task)
		}
		if !sandboxHeld {
			return nil
		}
		newlyStopped, err := stopSandboxProcessesAuthorized(task.WorktreePath, processAuthorization)
		if err != nil {
			return err
		}
		stopped = append(stopped, newlyStopped...)
		return nil
	}); err != nil {
		return result, s.teardownIncomplete(ctx, db, project, task, "Task process shutdown", err)
	}
	if sandboxHeld && task.State == store.StateMerged && !discardable && (task.MergeMode == "pr" || task.MergeMode == "no-mistakes" || project.IsWorkspace()) {
		if err := s.runIntentStep(ctx, db, intent, "orphan.snapshot", func() error {
			if !project.IsWorkspace() {
				return snapshotPROrphan(ctx, db, project, task)
			}
			members, err := s.workspaceMembers(ctx, db, project, task)
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
			newlyStopped, err := stopTaskScratchProcessesAuthorized(home, project, task, processAuthorization)
			if err != nil {
				return err
			}
			stopped = append(stopped, newlyStopped...)
		}
		return removeTaskScratch(home, project, task)
	}); err != nil {
		return result, s.teardownIncomplete(ctx, db, project, task, "Task scratch removal", err)
	}
	if inPlace {
		if err := s.runIntentStep(ctx, db, intent, "in_place.output", func() error {
			baselinePath := inPlaceBaselinePath(*plan)
			if _, statErr := os.Stat(baselinePath); os.IsNotExist(statErr) && task.Launches == 0 {
				return nil
			} else if statErr != nil {
				return statErr
			}
			output, outputErr := captureInPlaceOutput(ctx, *plan, task.WorktreePath)
			if outputErr != nil {
				var failure *axi.Error
				if !errors.As(outputErr, &failure) || failure.Code != "in_place_scope_violation" {
					return outputErr
				}
				return recordInPlaceScopeNotice(ctx, db, project, task, output)
			}
			return nil
		}); err != nil {
			return result, s.teardownIncomplete(ctx, db, project, task, "in-place output capture", err)
		}
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
		if inPlace {
			return releaseInPlaceLeases(ctx, db, project, task)
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
	if project.IsWorkspace() {
		err = s.runIntentStep(ctx, db, intent, "branch.remove", func() error {
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
				// A PR branch advanced after its external merge stays at its
				// existing tip even when the Sandbox can be safely released.
				if task.MergeMode == "pr" || task.MergeMode == "no-mistakes" {
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
					targetRef, targetErr := mergedTaskTargetRef(ctx, project, task)
					if targetErr != nil {
						return targetErr
					}
					if _, mergeErr := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", ref, targetRef); mergeErr == nil {
						merged = true
					}
					if task.State == store.StateMerged && (task.MergeMode == "pr" || task.MergeMode == "no-mistakes") {
						observation, observationErr := db.LatestPRObservation(ctx, task.ID)
						if observationErr != nil && !store.IsNotFound(observationErr) {
							return observationErr
						}
						if observationErr == nil && observation.State == "MERGED" {
							if sha != observation.HeadSHA {
								return nil // Preserve work added after the merged PR head.
							}
							merged = true // Squash and rebase merges need not contain the branch ref.
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
	err = s.runIntentStep(ctx, db, intent, "task.torn_down", func() error {
		if discardable {
			return db.TransitionAfterApproval(ctx, task.ID, task.State, store.StateTornDown, "user", "Task Sandbox discarded with User approval", "discard")
		}
		if inPlace && strings.TrimSpace(quote) != "" && (task.State == store.StateFailed || task.State == store.StateLost) {
			return db.TransitionInPlacePreserved(ctx, task, task.State, "in-place source preserved after User chose to stop recovery", quote)
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
	if err := db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
		return result, err
	}
	return result, nil
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
