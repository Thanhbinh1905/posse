package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) land(ctx *axi.Context, args []string) (returnErr error) {
	parsed, err := parseArgs("land", args, map[string]flagSpec{"merge": {boolean: true}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("land requires one Task id")
	}
	db, _, err := s.openDB()
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
	if task.Type != "ship" {
		return axi.Failure("land_refused", "only Ship Tasks can be Landed", false)
	}
	if parsed.Bool("merge") {
		if task.State != store.StateDone && task.State != store.StateLanding {
			return axi.Failure("land_refused", "Task must be done or landing before Land", false)
		}
		if task.AutonomyLand != "auto" && strings.TrimSpace(parsed.Flags["user-approved"]) == "" {
			mergeKind := "local merge"
			if task.LandingMode == "pr" || task.LandingMode == "no-mistakes" {
				mergeKind = "pull request merge"
			}
			return axi.Failure("land_approval_required", mergeKind+" needs User approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
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
	if task.Type != "ship" {
		return axi.Failure("land_refused", "only Ship Tasks can be Landed", false)
	}
	if project.IsWorkspace() {
		return s.landWorkspace(ctx, db, project, cfg, task, parsed.Bool("merge"), strings.TrimSpace(parsed.Flags["user-approved"]))
	}
	if task.LandingMode == "pr" || task.LandingMode == "no-mistakes" {
		return s.landPullRequest(ctx, db, project, cfg, task, parsed.Bool("merge"), strings.TrimSpace(parsed.Flags["user-approved"]))
	}
	var intent store.Intent
	hasIntent := false
	startLandIntent := func() error {
		if hasIntent {
			return nil
		}
		var intentErr error
		intent, intentErr = s.startTaskIntent(ctx.Context, db, project.ID, task.ID, "land --merge")
		if intentErr == nil {
			hasIntent = true
		}
		return intentErr
	}
	runLandStep := func(step string, action func() error) error {
		if !hasIntent {
			return action()
		}
		return s.runIntentStep(ctx.Context, db, intent, step, action)
	}
	finishLandIntent := func() error {
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
		if err := settleFailedLandIntent(ctx.Context, db, project, task, intent); err != nil {
			if returnErr == nil {
				returnErr = err
			}
		}
		hasIntent = false
	}()
	if task.LandingMode != "local" {
		return axi.Failure("land_refused", "unknown Landing Mode "+task.LandingMode, false)
	}
	if parsed.Bool("merge") {
		if task.State != store.StateDone && task.State != store.StateLanding {
			return axi.Failure("land_refused", "Task must be done or landing before Land", false)
		}
		quote := strings.TrimSpace(parsed.Flags["user-approved"])
		if task.AutonomyLand != "auto" && quote == "" {
			return axi.Failure("land_approval_required", "local merge needs User approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
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
		if parsed.Bool("merge") {
			if err := startLandIntent(); err != nil {
				return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
			}
		}
		if err := runLandStep("gate.record", func() error { return db.SetTaskGatedSHA(ctx.Context, task.ID, gatedSHA) }); err != nil {
			return err
		}
		if err := runLandStep("gate.run", func() error { return runGates(ctx.Context, cfg, task) }); err != nil {
			_, noticeErr := db.CreateNotice(ctx.Context, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "gate_failed", Summary: task.Title + ": " + truncate(err.Error(), 240), DataJSON: `{}`})
			if noticeErr != nil {
				return noticeErr
			}
			if finishErr := finishLandIntent(); finishErr != nil {
				return finishErr
			}
			_ = s.deliverNotices(ctx.Context, db, project)
			return axi.Failure("gate_failed", err.Error(), false, "Fix the gate failure on the Task branch, then run `posse send "+taskIDString(task.Seq)+" <instruction>`")
		}
		if err := runLandStep("task.landing", func() error {
			return db.Transition(ctx.Context, task.ID, store.StateDone, store.StateLanding, "cli", "Gate passed and local fast-forward is ready")
		}); err != nil {
			return err
		}
		if err := runLandStep("notice.create", func() error {
			_, noticeErr := db.CreateNotice(ctx.Context, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "land_ready", Summary: task.Title + " is ready to merge locally", DataJSON: `{}`})
			return noticeErr
		}); err != nil {
			return err
		}
		if err := s.deliverNotices(ctx.Context, db, project); err != nil {
			return err
		}
		task.State = store.StateLanding
		if !parsed.Bool("merge") {
			if err := s.regenerateProjects(ctx.Context, db); err != nil {
				return err
			}
			return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "landing"}, {Key: "help", Value: []any{"Run `posse land " + taskIDString(task.Seq) + " --merge --user-approved \"<User's words>\"` to merge"}}})
		}
	}
	if task.State != store.StateLanding {
		return axi.Failure("land_refused", "Task must be done or landing before Land", false, "Run `posse show "+taskIDString(task.Seq)+"` to inspect its state")
	}
	if !parsed.Bool("merge") {
		return axi.Failure("land_state_invalid", "Task is already waiting for its local merge", false, "Run `posse land "+taskIDString(task.Seq)+" --merge`")
	}
	if err := startLandIntent(); err != nil {
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
		if err := db.Transition(ctx.Context, task.ID, store.StateLanding, store.StateDone, "cli", "Task branch moved after the Gate passed"); err != nil {
			return err
		}
		if err := finishLandIntent(); err != nil {
			return err
		}
		return axi.Failure("branch_moved", "Task branch moved after the Gate passed", false, "Run `posse land "+taskIDString(task.Seq)+"` again to re-gate")
	}
	quote := strings.TrimSpace(parsed.Flags["user-approved"])
	if quote != "" {
		if err := runLandStep("approval.record", func() error { return db.RecordApproval(ctx.Context, task.ID, "merge", quote) }); err != nil {
			return err
		}
	}
	if err := runLandStep("merge", func() error { return mergeLocal(ctx.Context, project, task) }); err != nil {
		return err
	}
	landedRef, err := gitOutput(ctx.Context, project.Root, "rev-parse", "refs/heads/"+project.DefaultBranch)
	if err != nil {
		return err
	}
	if err := runLandStep("landed_ref.record", func() error { return db.UpdateTaskLanding(ctx.Context, task.ID, task.PRURL, landedRef) }); err != nil {
		return err
	}
	if err := runLandStep("task.landed", func() error {
		return db.Transition(ctx.Context, task.ID, store.StateLanding, store.StateLanded, "cli", "Local fast-forward completed at "+landedRef)
	}); err != nil {
		return err
	}
	if err := finishLandIntent(); err != nil {
		return err
	}
	if _, err := s.syncProjectRoot(ctx.Context, db, project, cfg, true); err != nil {
		return err
	}
	task.State = store.StateLanded
	if shouldAutoUnsaddleLanded(cfg) {
		result, err := s.unsaddleTask(ctx.Context, db, project, cfg, task, false, "")
		if err != nil {
			return err
		}
		return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "landed"}, {Key: "teardown", Value: "torn-down"}, {Key: "closed_panes", Value: result.Panes.Closed}, {Key: "foreign_panes", Value: result.Panes.Foreign}, {Key: "branch_removed", Value: result.BranchRemoved}, {Key: "stopped_processes", Value: result.StoppedProcesses}, {Key: "help", Value: []any{"The Task Report remains in its Project record"}}})
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "landed"}, {Key: "landed_ref", Value: landedRef}, {Key: "help", Value: []any{"Run `posse unsaddle " + taskIDString(task.Seq) + "` to release the Mount"}}})
}

func settleFailedLandIntent(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent) error {
	current, err := db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return err
	}
	if current.State == store.StateLanding {
		if current.LandingMode == "pr" || current.LandingMode == "no-mistakes" {
			return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
		}
		merged := false
		if current.GatedSHA != "" {
			_, mergeErr := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", current.GatedSHA, "refs/heads/"+project.DefaultBranch)
			merged = mergeErr == nil
		}
		if merged {
			landedRef, err := gitOutput(ctx, project.Root, "rev-parse", "refs/heads/"+project.DefaultBranch)
			if err != nil {
				return err
			}
			if err := db.UpdateTaskLanding(ctx, current.ID, current.PRURL, landedRef); err != nil {
				return err
			}
			if err := db.Transition(ctx, current.ID, store.StateLanding, store.StateLanded, "cli", "Local fast-forward completed at "+landedRef); err != nil {
				return err
			}
		} else {
			if err := db.Transition(ctx, current.ID, store.StateLanding, store.StateDone, "cli", "Interrupted Land did not complete its merge"); err != nil {
				return err
			}
			if err := db.ClearTaskGatedSHA(ctx, current.ID); err != nil {
				return err
			}
		}
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
		return axi.Failure("land_refused", "Task has no worktree or branch", false)
	}
	if _, err := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", "refs/heads/"+project.DefaultBranch, "refs/heads/"+task.Branch); err != nil {
		return axi.Failure("needs_rebase", "Task branch is not a fast-forward of the Project default branch", false, "Rebase the Task branch on "+project.DefaultBranch+" and signal done again")
	}
	status, err := gitOutput(ctx, task.WorktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return axi.Failure("land_refused", "Task worktree is not clean", false, "Commit or discard the remaining changes, then signal done again")
	}
	return nil
}

func mergeLocal(ctx context.Context, project store.Project, task store.Task) error {
	worktrees, err := gitOutput(ctx, project.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	checkedOut := false
	for _, line := range strings.Split(worktrees, "\n") {
		if strings.TrimSpace(line) == "branch refs/heads/"+project.DefaultBranch {
			checkedOut = true
			break
		}
	}
	if checkedOut {
		branch, err := gitOutput(ctx, project.Root, "branch", "--show-current")
		if err != nil {
			return err
		}
		if branch != project.DefaultBranch {
			return axi.Failure("land_target_busy", "default branch is checked out in a different worktree", false, "Switch the Project checkout to "+project.DefaultBranch+" and retry")
		}
		status, err := gitOutput(ctx, project.Root, "status", "--porcelain")
		if err != nil {
			return err
		}
		if status != "" {
			return axi.Failure("land_target_busy", "Project checkout has uncommitted changes", false, "Clean the Project checkout and retry")
		}
		if _, err := gitOutput(ctx, project.Root, "merge", "--ff-only", task.GatedSHA); err != nil {
			return axi.Failure("needs_rebase", "local fast-forward merge failed", false, err.Error())
		}
		return nil
	}
	output, err := commandOutputArgs(ctx, "", "git", "-C", project.Root, "fetch", ".", task.GatedSHA+":refs/heads/"+project.DefaultBranch)
	if err != nil {
		return axi.Failure("needs_rebase", "local fast-forward update failed", false, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *Service) teardown(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("unsaddle", args, map[string]flagSpec{"discard": {boolean: true}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("unsaddle requires one Task id")
	}
	db, _, err := s.openDB()
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
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	discardable := task.State == store.StateFailed || task.State == store.StateLost || task.State == store.StateStalled
	if !discardable && task.State != store.StateLanded && task.State != store.StateReported {
		return axi.Failure("teardown_refused", "Task in state "+string(task.State)+" cannot be torn down", false)
	}
	if task.State == store.StateLanded && task.LandingMode == "pr" {
		safe, safetyErr := safePRMergeTeardown(ctx.Context, db, project, task)
		if safetyErr != nil {
			return safetyErr
		}
		if !safe {
			return axi.Failure("teardown_refused", "merged PR has unmerged Task work; keep the Rider and publish follow-up work before teardown", false)
		}
	}
	if discardable && !parsed.Bool("discard") {
		return axi.Failure("teardown_refused", "unlanded work requires --discard and User approval", false)
	}
	quote := strings.TrimSpace(parsed.Flags["user-approved"])
	if discardable && quote == "" {
		return axi.Failure("teardown_refused", "discard requires a recorded User approval quote", false, "Pass `--discard --user-approved \"<User's words>\"`")
	}
	result, err := s.unsaddleTask(ctx.Context, db, project, cfg, task, discardable, quote)
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "torn-down"}, {Key: "closed_panes", Value: result.Panes.Closed}, {Key: "foreign_panes", Value: result.Panes.Foreign}, {Key: "branch_removed", Value: result.BranchRemoved}, {Key: "stopped_processes", Value: result.StoppedProcesses}, {Key: "help", Value: []any{"Run `posse remuda` to inspect released Mounts"}}})
}

func (s *Service) unsaddleTask(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, discardable bool, quote string) (unsaddleResult, error) {
	result := unsaddleResult{}
	intent, err := s.startTaskIntentWithPayload(ctx, db, project.ID, task.ID, "unsaddle", marshalJSON(map[string]any{"discard": discardable}))
	if err != nil {
		return result, axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	defer func() { _ = db.FinishIntent(ctx, intent.ID, intent.ProcessID) }()
	if discardable {
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
		if err := s.runIntentStep(ctx, db, intent, "approval.record", func() error { return db.RecordApprovalWithBranch(ctx, task.ID, "discard", quote, branchSHA) }); err != nil {
			return result, err
		}
	}
	var paneResult teardownPanes
	err = s.runIntentStep(ctx, db, intent, "panes.close", func() error {
		var closeErr error
		paneResult, closeErr = s.closeTaskPanes(ctx, project, task)
		return closeErr
	})
	if err != nil {
		return result, s.unsaddleIncomplete(ctx, db, project, task, err)
	}
	var killed []string
	err = s.runIntentStep(ctx, db, intent, "mount.release", func() error {
		if !discardable && task.State == store.StateLanded && task.LandingMode == "pr" {
			// The Rider's pane has been closed. Recheck after all writes stop,
			// immediately before the destructive reset of its Mount.
			safe, safetyErr := safePRMergeTeardown(ctx, db, project, task)
			if safetyErr != nil {
				return safetyErr
			}
			if !safe {
				return axi.Failure("teardown_refused", "Task work changed before Mount release; preserve it", false)
			}
		}
		var releaseErr error
		killed, releaseErr = releaseMount(ctx, db, project, task, cfg.Remuda.Clean)
		return releaseErr
	})
	if err != nil {
		if task.State == store.StateLanded && task.LandingMode == "pr" {
			safe, safetyErr := safePRMergeTeardown(ctx, db, project, task)
			if safetyErr == nil && !safe {
				// Work written after the first cleanliness check belongs to
				// the Rider, not to teardown. Restore its reportable state.
				if restoreErr := restoreMergedTaskWithWork(ctx, db, project, task); restoreErr != nil {
					return result, errors.Join(err, restoreErr)
				}
			}
		}
		return result, s.unsaddleIncomplete(ctx, db, project, task, err)
	}
	result.Panes = paneResult
	result.StoppedProcesses = killed
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
			return result, s.unsaddleIncomplete(ctx, db, project, task, err)
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
				return result, s.unsaddleIncomplete(ctx, db, project, task, err)
			}
		}
	} else {
		if task.Branch != "" {
			err = s.runIntentStep(ctx, db, intent, "branch.remove", func() error {
				// A PR branch advanced after its external merge stays at its
				// existing tip even when the Mount can be safely released.
				if task.LandingMode == "pr" {
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
					if _, mergeErr := gitOutput(ctx, project.Root, "merge-base", "--is-ancestor", ref, "refs/heads/"+project.DefaultBranch); mergeErr == nil {
						if _, err := gitOutput(ctx, project.Root, "update-ref", "-d", ref, sha); err != nil {
							return err
						}
						result.BranchRemoved = true
					}
				}
				return nil
			})
			if err != nil {
				return result, s.unsaddleIncomplete(ctx, db, project, task, err)
			}
		}
	}
	err = s.runIntentStep(ctx, db, intent, "task.torn_down", func() error {
		if discardable {
			return db.TransitionAfterApproval(ctx, task.ID, task.State, store.StateTornDown, "user", "Task Mount discarded with User approval", "discard")
		}
		return db.Transition(ctx, task.ID, task.State, store.StateTornDown, "cli", "Task Mount released after completion")
	})
	if err != nil {
		return result, s.unsaddleIncomplete(ctx, db, project, task, err)
	}
	if err := s.refreshWorkerDisplay(ctx, db, project, 0); err != nil {
		return result, err
	}
	if err := s.regenerateProjects(ctx, db); err != nil {
		return result, err
	}
	if err := db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
		return result, err
	}
	return result, nil
}

func isMissingGitRef(err error) bool {
	return strings.Contains(err.Error(), "unknown revision") || strings.Contains(err.Error(), "Needed a single revision") || strings.Contains(err.Error(), "not a valid object name")
}
