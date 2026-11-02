package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) prRelease(ctx *axi.Context, args []string) error {
	return s.stewardCommand(ctx, "release", args, map[string]flagSpec{"policy": {}}, func(db *store.DB, target stewardTarget, parsed parsedArgs) error {
		if target.project.IsWorkspace() {
			return prReleaseRefused("executor retention is Task-wide; workspace release requires preservation proof for every changed Member", false)
		}
		policy := parsed.Flags["policy"]
		if policy != "retain" && policy != "suspend" && policy != "release" {
			return axi.Usage("pr release requires --policy retain|suspend|release")
		}
		if target.task.State != store.StateDone && target.task.State != store.StateMerging {
			return prReleaseRefused("executor policy can change only after the Worker signals done", false)
		}
		pending, err := db.HasPendingTaskInstructions(ctx.Context, target.task.ID)
		if err != nil {
			return err
		}
		uncertain, err := db.UncertainTaskMessages(ctx.Context, target.project.ID, target.task.ID, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if pending || len(uncertain) > 0 {
			return prReleaseRefused("pending Worker instructions could contain output that has not been preserved", true)
		}
		if policy == "retain" {
			held, err := taskSandboxHeldBy(ctx.Context, db, target.task)
			if err != nil {
				return err
			}
			if !held {
				return prReleaseRefused("the Task Sandbox is not held; relaunch the Task before selecting retain", false)
			}
			snapshot, err := s.snapshot(ctx.Context)
			if err != nil {
				return err
			}
			pane, found := findTaskPane(snapshot.Panes, target.task)
			if !found || pane.Agent == "" {
				return prReleaseRefused("the executor is already absent; relaunch the Task before selecting retain", false)
			}
			if pane.AgentStatus != "idle" && pane.AgentStatus != "done" {
				return prReleaseRefused("Worker status is "+pane.AgentStatus+"; wait for a settled turn before retaining it", true)
			}
			if err := s.recordExecutorPolicy(ctx.Context, db, target, policy, "Executor and Sandbox retained"); err != nil {
				return err
			}
			return printExecutorPolicy(ctx, target, policy, false)
		}
		if policy == "release" {
			observation, err := s.verifyDeliveryPreserved(ctx.Context, db, target)
			if err != nil {
				return err
			}
			if err := s.releaseExecutor(ctx.Context, db, target, target.cfg); err != nil {
				var failure *axi.Error
				if errors.As(err, &failure) && failure.Code == "pr_release_refused" {
					return err
				}
				return axi.Failure("pr_release_incomplete", "executor release did not finish: "+err.Error(), true, "Retry `posse pr release "+target.taskLabel()+" --policy release` after repairing the named resource")
			}
			if err := s.recordExecutorPolicy(ctx.Context, db, target, policy, "Executor stopped; Sandbox released after verifying PR head "+observation.HeadSHA); err != nil {
				return err
			}
			return printExecutorPolicy(ctx, target, policy, true)
		}
		held, err := taskSandboxHeldBy(ctx.Context, db, target.task)
		if err != nil {
			return err
		}
		if !held {
			return prReleaseRefused("the Task Sandbox is not held; release cannot be changed to suspend", false)
		}
		if err := s.suspendExecutor(ctx.Context, target.task); err != nil {
			return err
		}
		if err := s.recordExecutorPolicy(ctx.Context, db, target, policy, "Executor stopped; Sandbox and branch retained"); err != nil {
			return err
		}
		return printExecutorPolicy(ctx, target, policy, false)
	})
}

func printExecutorPolicy(ctx *axi.Context, target stewardTarget, policy string, sandboxReleased bool) error {
	executor := map[string]string{"retain": "retained", "suspend": "stopped", "release": "stopped"}[policy]
	return ctx.Print(axi.Object{
		{Key: "task", Value: taskIDString(target.task.Seq)},
		{Key: "delivery", Value: target.prURL},
		{Key: "policy", Value: policy},
		{Key: "executor", Value: executor},
		{Key: "sandbox_released", Value: sandboxReleased},
		{Key: "help", Value: []any{"The Task Report, Delivery, PR watch, and operation history remain in Posse custody"}},
	})
}

func prReleaseRefused(reason string, retryable bool) error {
	return axi.Failure("pr_release_refused", reason, retryable, "Preserve the Task branch, Sandbox, Reports, and Delivery; retry only after the uncertainty is resolved")
}

func (s *Service) verifyDeliveryPreserved(ctx context.Context, db *store.DB, target stewardTarget) (store.PRObservation, error) {
	observation, err := s.currentOpenDelivery(ctx, db, target)
	if err != nil {
		return store.PRObservation{}, prReleaseRefused("PR preservation is uncertain: "+err.Error(), true)
	}
	if target.task.Branch == "" || target.task.WorktreePath == "" {
		return store.PRObservation{}, prReleaseRefused("the Task branch or Sandbox path is missing", false)
	}
	branchRef := "refs/heads/" + target.task.Branch
	branchHead, err := gitOutput(ctx, target.project.Root, "rev-parse", branchRef)
	if err != nil {
		return store.PRObservation{}, prReleaseRefused("cannot verify the retained Task branch: "+err.Error(), true)
	}
	remoteHead, err := remoteBranchHead(ctx, target.project.Root, branchRef)
	if err != nil {
		return store.PRObservation{}, prReleaseRefused("cannot verify the remote Task branch: "+err.Error(), true)
	}
	if branchHead != remoteHead || observation.HeadSHA != branchHead {
		return store.PRObservation{}, prReleaseRefused(fmt.Sprintf("preservation is uncertain: local branch %s, remote branch %s, and PR head %s must match", shortSHA(branchHead), shortSHA(remoteHead), shortSHA(observation.HeadSHA)), true)
	}
	if _, err := os.Stat(target.task.WorktreePath); err == nil {
		status, err := gitStatusOutput(ctx, target.task.WorktreePath, "--porcelain", "--ignored")
		if err != nil || status != "" {
			cause := "the Task Sandbox has uncommitted or ignored output"
			if err != nil {
				cause = "cannot inspect the Task Sandbox: " + err.Error()
			}
			return store.PRObservation{}, prReleaseRefused("preservation is uncertain: "+cause, true)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return store.PRObservation{}, prReleaseRefused("cannot inspect the Task Sandbox: "+err.Error(), true)
	}
	plan, err := target.task.AdmittedPlan()
	if err != nil {
		return store.PRObservation{}, admittedPlanFailure(err)
	}
	if plan != nil && plan.Review == "full-pipeline" {
		candidate, err := db.LatestValidationCandidate(ctx, target.task.ID, "")
		if err != nil || candidate.State != store.ValidationCandidatePromoted || candidate.PromotedSHA != branchHead {
			return store.PRObservation{}, prReleaseRefused("the current full-pipeline candidate is not confirmed as promoted", true)
		}
	}
	return observation, nil
}

func (s *Service) recordExecutorPolicy(ctx context.Context, db *store.DB, target stewardTarget, policy, detail string) error {
	_, err := db.RecordDeliveryOperation(ctx, store.DeliveryOperation{
		DeliveryID: target.delivery.ID,
		Operation:  "executor_policy",
		Actor:      target.actor,
		ActorPane:  target.actorPane,
		HeadSHA:    target.task.GatedSHA,
		Subject:    policy,
		Outcome:    "succeeded",
		Detail:     detail,
	})
	return err
}

func (s *Service) suspendExecutor(ctx context.Context, task store.Task) error {
	if s.Herdr == nil {
		return prReleaseRefused("Herdr is unavailable, so the executor cannot be verified", true)
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	pane, found := findTaskPane(snapshot.Panes, task)
	if !found || pane.Agent == "" {
		return nil
	}
	if pane.AgentStatus != "idle" && pane.AgentStatus != "done" {
		return prReleaseRefused("Worker status is "+pane.AgentStatus+"; wait for a settled turn before suspending it", true)
	}
	return s.stopPaneAgent(ctx, pane.PaneID, pane.Agent)
}

func (s *Service) releaseExecutor(ctx context.Context, db *store.DB, target stewardTarget, cfg config.Config) (returnErr error) {
	project, task := target.project, target.task
	intent, err := s.startTaskIntentWithPayload(ctx, db, project.ID, task.ID, "pr-release", `{}`)
	if err != nil {
		return err
	}
	defer func() {
		if returnErr == nil {
			returnErr = s.finishTaskIntent(context.Background(), db, task.ID)
		}
	}()
	sandbox, err := db.SandboxByTask(ctx, task.ID)
	if err != nil && !store.IsNotFound(err) {
		return err
	}
	if task.SandboxID != 0 && (store.IsNotFound(err) || sandbox.ID != task.SandboxID || sandbox.TaskID != task.ID || sandbox.State != "held" && sandbox.State != "releasing") {
		return prReleaseRefused("the Task Sandbox ownership cannot be verified", true)
	}
	if err == nil && sandbox.TaskID == task.ID && task.SandboxID == 0 {
		return prReleaseRefused("the Task's recorded Sandbox does not match its held Mount", true)
	}
	sandboxHeld := err == nil && sandbox.State == "held"
	sandboxNeedsRelease := err == nil && (sandboxHeld || sandbox.State == "releasing")
	var authorization *sandboxProcessAuthorization
	if sandboxHeld {
		authorization = newSandboxProcessAuthorization()
		defer authorization.Close()
		if err := s.verifySandboxForegroundOwnershipWithAuthorization(ctx, db, project, task, authorization); err != nil {
			return err
		}
	}
	if _, err := s.planTaskPaneTeardown(ctx, project, task); err != nil {
		return err
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	if pane, found := findTaskPane(snapshot.Panes, task); found && pane.Agent != "" && pane.AgentStatus != "idle" && pane.AgentStatus != "done" {
		return prReleaseRefused("Worker status is "+pane.AgentStatus+"; wait for a settled turn before releasing it", true)
	}
	if err := releaseIntentStep(ctx, db, intent, "executor.stop", func() error {
		guard := taskProcessSignalGuard(nil)
		if authorization != nil {
			guard = authorization.guardSignal
		}
		preserved, err := stopTaskProcessContainmentWithGuard(ctx, task, guard)
		if err != nil {
			return err
		}
		excluded := taskProcessDetailsPIDSet(preserved)
		if sandboxHeld {
			if _, err := stopSandboxProcessesAuthorized(task.WorktreePath, authorization, excluded); err != nil {
				return err
			}
		}
		home, err := s.homePath()
		if err != nil {
			return err
		}
		_, err = stopTaskScratchProcessesAuthorized(home, project, task, authorization, excluded)
		return err
	}); err != nil {
		return err
	}
	if _, err := s.waitTaskPanesGone(ctx, project, task); err != nil {
		return err
	}
	s.relabelProjectTabs(ctx, db, project)
	if _, err := s.verifyDeliveryPreserved(ctx, db, target); err != nil {
		return prReleaseRefused("preservation became uncertain while stopping the executor: "+err.Error(), true)
	}
	if sandboxNeedsRelease {
		if err := releaseIntentStep(ctx, db, intent, "sandbox.release", func() error {
			_, err := releaseSandbox(ctx, db, project, task, cfg.Pool.Clean, false)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func releaseIntentStep(ctx context.Context, db *store.DB, intent store.Intent, step string, action func() error) error {
	current, err := db.IntentByTask(ctx, intent.TaskID)
	if err != nil {
		return err
	}
	prior := strings.TrimPrefix(current.Step, "resuming:")
	if prior == "done:"+step {
		return nil
	}
	if err := db.UpdateIntentStep(ctx, intent.ID, intent.ProcessID, "in_progress:"+step); err != nil {
		return err
	}
	crashIntentAt(intent.Command, "before", step)
	if err := action(); err != nil {
		return err
	}
	if err := db.UpdateIntentStep(ctx, intent.ID, intent.ProcessID, "done:"+step); err != nil {
		return err
	}
	crashIntentAt(intent.Command, "after", step)
	return nil
}
