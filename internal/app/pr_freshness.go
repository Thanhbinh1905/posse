package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

type prTargetFreshness struct {
	Branch string
	Tip    string
	Fresh  bool
	Local  bool
	Root   string
}

func inspectPRTargetFreshness(ctx context.Context, root, targetBranch, headSHA string) (prTargetFreshness, error) {
	freshness, err := fetchPRTargetTip(ctx, root, targetBranch)
	if err != nil {
		return freshness, err
	}
	return freshness.of(ctx, root, headSHA)
}

// fetchPRTargetTip refreshes origin/<target> and records its tip.
func fetchPRTargetTip(ctx context.Context, root, targetBranch string) (prTargetFreshness, error) {
	freshness := prTargetFreshness{Branch: targetBranch, Root: root}
	targetRef := "refs/remotes/origin/" + targetBranch
	refspec := "+refs/heads/" + targetBranch + ":" + targetRef
	output, err := gitFetch(ctx, root, "origin", refspec)
	if err != nil {
		details := strings.TrimSpace(output)
		if details == "" {
			details = err.Error()
		}
		return freshness, axi.Failure("target_branch_unavailable", "could not refresh PR target branch "+targetBranch, true, details)
	}
	if freshness.Tip, err = gitOutput(ctx, root, "rev-parse", "--verify", targetRef+"^{commit}"); err != nil {
		return freshness, axi.Failure("target_branch_unavailable", "could not inspect PR target branch "+targetBranch, true, err.Error())
	}
	return freshness, nil
}

// of reports whether headSHA contains the recorded target tip without
// refreshing it.
func (freshness prTargetFreshness) of(ctx context.Context, root, headSHA string) (prTargetFreshness, error) {
	if strings.TrimSpace(headSHA) == "" {
		return freshness, fmt.Errorf("pull request has no head commit to check against origin/%s", freshness.Branch)
	}
	_, err := gitOutput(ctx, root, "merge-base", "--is-ancestor", freshness.Tip, headSHA)
	freshness.Fresh = err == nil
	return freshness, nil
}

// targetSync is the result of asking a behind PR to merge its target: the
// Worker has a queued instruction only when its Task heads the merge queue.
type targetSync struct {
	Queued bool
	Place  store.MergeQueuePlace
}

func prBranchBehindFailure(taskSeq int, headSHA string, freshness prTargetFreshness, sync targetSync) error {
	message := fmt.Sprintf("pull request head %s does not contain current target commit %s from origin/%s", headSHA, freshness.Tip, freshness.Branch)
	if sync.Queued {
		return axi.Failure("pr_branch_behind", message+"; Posse queued the Worker an instruction to merge origin/"+freshness.Branch, false, queuedTargetSyncHelp(taskSeq))
	}
	if sync.Place.Queued() {
		return axi.Failure(
			"pr_branch_behind",
			fmt.Sprintf("%s; the Task is at position %d of %d in the origin/%s merge queue behind %s", message, sync.Place.Position, sync.Place.Length, freshness.Branch, taskIDString(sync.Place.HeadSeq)),
			false,
			"Wait until the merge queue promotes "+taskIDString(taskSeq)+"; Posse then asks its Worker to merge origin/"+freshness.Branch+". Do not send a merge-target instruction",
		)
	}
	return axi.Failure(
		"pr_branch_behind",
		message,
		false,
		"Merge origin/"+freshness.Branch+" into the Task branch, republish, and report done before retrying `posse merge "+taskIDString(taskSeq)+"`; never rebase a pushed branch",
	)
}

func queuedTargetSyncHelp(taskSeq int) string {
	return "Wait for the Worker's done Signal, then run `posse merge " + taskIDString(taskSeq) + "` again; do not send a duplicate merge-target instruction"
}

// pendingTargetSyncFailure explains a Merge that found no gated head because
// Posse already queued the Worker an instruction to merge the PR target.
func pendingTargetSyncFailure(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	if task.Type != "change" || task.MergeMode != "pr" {
		return nil
	}
	headSHA, err := db.OutstandingValidationFixHead(ctx, task.ID, "freshness")
	if err != nil || headSHA == "" {
		return err
	}
	branch := taskTargetBranch(project, task)
	freshness, err := inspectPRTargetFreshness(ctx, project.Root, branch, headSHA)
	if err != nil {
		return err
	}
	return prBranchBehindFailure(task.Seq, headSHA, freshness, targetSync{Queued: true})
}

func workingPRBehindFailure(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task) error {
	if task.State != store.StateWorking || task.Type != "change" || task.PRURL == "" || task.MergeMode != "pr" {
		return nil
	}
	if err := pendingTargetSyncFailure(ctx, db, project, task); err != nil {
		return err
	}
	if !cfg.Defaults.RequireCurrentTarget {
		return nil
	}
	observation, err := db.LatestPRObservation(ctx, task.ID)
	if store.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if observation.PRURL != task.PRURL || observation.State != "OPEN" || observation.HeadSHA == "" {
		return nil
	}
	freshness, err := inspectPRTargetFreshness(ctx, project.Root, taskTargetBranch(project, task), observation.HeadSHA)
	if err != nil {
		return err
	}
	if freshness.Fresh {
		return nil
	}
	return prBranchBehindFailure(task.Seq, observation.HeadSHA, freshness, targetSync{})
}

// queuePRTargetSyncInstruction enters a behind done or merging Task in its
// target's merge queue and queues the merge-target instruction only for the
// queue head, deduplicated against an earlier one (ADR 0031).
func (s *Service) queuePRTargetSyncInstruction(ctx context.Context, db *store.DB, task store.Task, member string, freshness prTargetFreshness, headSHA string) (targetSync, error) {
	if task.State != store.StateDone && task.State != store.StateMerging {
		return targetSync{}, nil
	}
	if freshness.Root != "" {
		queuedHead := headSHA
		checkedHead := headSHA
		if greenhouseFullPipelineTaskWithConfig(task, config.Config{}, member) {
			released, err := taskExecutorReleased(ctx, db, task.ID)
			if err != nil {
				return targetSync{}, err
			}
			if !released {
				worktreePath := task.WorktreePath
				if member != "" {
					repos, err := db.TaskRepos(ctx, task.ID)
					if err != nil {
						return targetSync{}, err
					}
					worktreePath = ""
					for _, repo := range repos {
						if repo.Repo == member {
							worktreePath = repo.WorktreePath
							break
						}
					}
					if worktreePath == "" {
						return targetSync{}, fmt.Errorf("workspace Task %d has no repository %q", task.Seq, member)
					}
				}
				if worktreePath != "" {
					currentHead, err := gitOutput(ctx, worktreePath, "rev-parse", "HEAD")
					if err != nil {
						return targetSync{}, err
					}
					checkedHead = currentHead
				}
			}
		}
		headSHA = checkedHead
		checked, err := freshness.of(ctx, freshness.Root, headSHA)
		if err != nil {
			return targetSync{}, err
		}
		if checked.Fresh {
			for _, head := range []string{headSHA, queuedHead} {
				if _, err := db.WithdrawQueuedFreshnessInstructionsForHead(ctx, task.ID, head); err != nil {
					return targetSync{}, err
				}
			}
			if err := db.DequeueMergeQueue(ctx, task.ID, member, "target_already_current", time.Now().UnixMilli()); err != nil {
				return targetSync{}, err
			}
			return targetSync{}, nil
		}
	}
	pending, err := hasCurrentTargetWork(ctx, db, task, member, freshness.Tip)
	if err != nil || pending {
		return targetSync{}, err
	}
	place, err := db.PlaceInMergeQueue(ctx, task.ID, member, freshness.Branch, time.Now().UnixMilli())
	if err != nil {
		return targetSync{}, err
	}
	if !place.Head() {
		return targetSync{Place: place}, nil
	}
	queued, err := queueTargetMergeInstruction(ctx, db, task, member, freshness, headSHA)
	if err != nil {
		return targetSync{}, err
	}
	if !queued {
		if err := db.DequeueMergeQueue(ctx, task.ID, member, "target_already_current", time.Now().UnixMilli()); err != nil {
			return targetSync{}, err
		}
		return targetSync{}, nil
	}
	if err := s.relaunchReleasedFullPipelineWorker(ctx, db, task, member); err != nil {
		return targetSync{}, err
	}
	return targetSync{Queued: true, Place: place}, nil
}

// queueConflictTargetSyncInstruction asks the Worker of a done or merging Task
// whose PR conflicts with its target to merge the target, without the merge
// queue. It applies when defaults.require_current_target is off, so only
// real conflicts need a Worker update (ADR 0019).
func (s *Service) queueConflictTargetSyncInstruction(ctx context.Context, db *store.DB, task store.Task, member string, freshness prTargetFreshness, headSHA string) error {
	if task.State != store.StateDone && task.State != store.StateMerging {
		return nil
	}
	queued, err := queueTargetMergeInstruction(ctx, db, task, member, freshness, headSHA)
	if err != nil || !queued {
		return err
	}
	return s.relaunchReleasedFullPipelineWorker(ctx, db, task, member)
}

func (s *Service) relaunchReleasedFullPipelineWorker(ctx context.Context, db *store.DB, task store.Task, member string) error {
	if !fullPipelineSelected(task, member) || s.Herdr == nil {
		return nil
	}
	released, err := taskExecutorReleased(ctx, db, task.ID)
	if err != nil || !released {
		return err
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	pane, found := findTaskPane(snapshot.Panes, task)
	if found && pane.Agent != "" {
		return nil
	}
	project, err := db.ProjectByID(ctx, task.ProjectID)
	if err != nil {
		return err
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return err
	}
	_, err = s.relaunchTask(ctx, db, home, project, cfg, task, "")
	return err
}

func taskExecutorReleased(ctx context.Context, db *store.DB, taskID int64) (bool, error) {
	operation, found, err := db.LatestAppliedTaskExecutorPolicy(ctx, taskID)
	if err != nil || !found {
		return false, err
	}
	return operation.Outcome == "succeeded" && operation.Subject == "release", nil
}

func hasCurrentTargetWork(ctx context.Context, db *store.DB, task store.Task, member, targetSHA string) (bool, error) {
	candidates, err := db.LiveValidationCandidates(ctx, task.ID)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		if candidate.Repo == member && candidate.TargetSHA == targetSHA {
			return true, nil
		}
	}
	decisions, err := db.PendingDecisions(ctx, task.ProjectID)
	if err != nil {
		return false, err
	}
	for _, decision := range decisions {
		if decision.TaskID == task.ID && decision.Kind == "review_cap" {
			return true, nil
		}
	}
	return false, nil
}

func queueTargetMergeInstruction(ctx context.Context, db *store.DB, task store.Task, member string, freshness prTargetFreshness, headSHA string) (bool, error) {
	if greenhouseFullPipelineTaskWithConfig(task, config.Config{}, member) && member == "" && task.WorktreePath != "" && (task.State == store.StateDone || task.State == store.StateMerging) {
		currentHead, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "HEAD")
		if err != nil {
			return false, err
		}
		checked, err := freshness.of(ctx, task.WorktreePath, currentHead)
		if err != nil {
			return false, err
		}
		if checked.Fresh {
			for _, head := range []string{currentHead, headSHA} {
				if _, err := db.WithdrawQueuedFreshnessInstructionsForHead(ctx, task.ID, head); err != nil {
					return false, err
				}
			}
			return false, nil
		}
	}
	if freshness.Root != "" {
		checked, err := freshness.of(ctx, freshness.Root, headSHA)
		if err != nil {
			return false, err
		}
		if checked.Fresh {
			_, err := db.WithdrawQueuedFreshnessInstructionsForHead(ctx, task.ID, headSHA)
			return false, err
		}
	}
	memberLabel := ""
	if member != "" {
		memberLabel = "Workspace member " + member + ": "
	}
	var instruction string
	if greenhouseFullPipelineTaskWithConfig(task, config.Config{}, member) {
		target := "origin/" + freshness.Branch
		if freshness.Local {
			target = freshness.Branch
		}
		instruction = fmt.Sprintf(
			"%sGreenhouse-reviewed head %s is missing current target commit %s from %s. Run `posse signal working \"Refreshing the target\"`, merge %s into the Task branch and resolve conflicts, then request review for the new head with `posse signal done \"<summary>\" --head <sha>` without --pr. After Greenhouse passes, publish the exact head with `posse publish \"<summary>\"` and report done with the PR URL.",
			memberLabel, headSHA, freshness.Tip, target, target,
		)
	} else if fullPipelineSelected(task, member) {
		target := "origin/" + freshness.Branch
		mergeTarget := "origin/" + freshness.Branch
		if freshness.Local {
			target = "local target " + freshness.Branch
			mergeTarget = freshness.Branch
		}
		publish := "posse publish"
		if member != "" {
			publish += " --repo " + member
		}
		instruction = fmt.Sprintf(
			"%sCandidate head %s is missing current target commit %s from %s, so its full-pipeline validation is stale. From the Task Sandbox, run `%s --refresh \"<summary>\"` to submit a fresh private candidate; Posse merges the current target into it. If the submission reports `candidate_target_conflict`, merge %s into the Task branch, resolve the conflicts and publish again. If the output names `review_carried_from`, the passed Review carries: in the candidate checkout run `posse validate test`, `posse validate docs` and `posse validate lint`, then `%s \"<summary>\"` there to promote. Otherwise signal done from the candidate checkout to request a Review Round.",
			memberLabel, headSHA, freshness.Tip, target, publish, mergeTarget, publish,
		)
	} else {
		target := "origin/" + freshness.Branch
		if freshness.Local {
			target = freshness.Branch
		}
		instruction = fmt.Sprintf(
			"%sPR head %s is missing current target commit %s from %s. Merge %s into the Task branch (never rebase a pushed branch), resolve any conflicts, republish the updated head through this Task's delivery mode, and signal done again.",
			memberLabel, headSHA, freshness.Tip, target, target,
		)
		if member != "" {
			instruction += " For publication, use `posse publish --repo " + member + " \"<summary>\"` and report done without `--pr`."
		} else {
			instruction += " For publication, use `posse publish \"<summary>\"` and report done with the PR URL."
		}
	}
	after := task.CreatedAt
	signals, err := db.TaskSignals(ctx, task.ID, 1)
	if err != nil {
		return false, err
	}
	if len(signals) > 0 && signals[0].Verb == "done" {
		after = signals[0].At
	}
	dedupeKey := fmt.Sprintf("freshness:%s:%s:%s:%d", member, headSHA, freshness.Tip, after)
	_, err = db.QueueValidationFixInstruction(ctx, task.ID, "freshness", headSHA, dedupeKey, instruction)
	return err == nil, err
}

func mergeRefusalIndicatesTargetMovement(err error) bool {
	message := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"not up to date",
		"branch is behind",
		"base branch was modified",
		"base branch has been modified",
		"base branch has changed",
		"base branch changed",
		"base branch is out of date",
		"target branch was modified",
		"target branch has changed",
		"target branch changed",
		"target branch is out of date",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

func (s *Service) refreshAfterPRMergeRefusal(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, member string, mergeErr error) error {
	if !cfg.Defaults.RequireCurrentTarget && !greenhouseFullPipelineTaskWithConfig(task, cfg, member) {
		return nil
	}
	if !mergeRefusalIndicatesTargetMovement(mergeErr) {
		return nil
	}
	freshness, err := inspectPRTargetFreshness(ctx, project.Root, taskTargetBranch(project, task), task.GatedSHA)
	if err != nil {
		return err
	}
	if freshness.Fresh {
		return nil
	}
	sync, err := s.queuePRTargetSyncInstruction(ctx, db, task, member, freshness, task.GatedSHA)
	if err != nil {
		return err
	}
	if task.State == store.StateMerging {
		if member == "" {
			if err := db.Transition(ctx, task.ID, store.StateMerging, store.StateWorking, "lead", "Pull request merge was refused because its target advanced"); err != nil {
				return err
			}
			if err := db.ClearTaskGatedSHA(ctx, task.ID); err != nil {
				return err
			}
		} else {
			repos, err := db.TaskRepos(ctx, task.ID)
			if err != nil {
				return err
			}
			var repo store.TaskRepo
			for _, candidate := range repos {
				if candidate.Repo == member {
					repo = candidate
					break
				}
			}
			if repo.Repo == "" {
				return fmt.Errorf("workspace Task %d has no repository %q", task.Seq, member)
			}
			if err := s.reopenWorkspaceTask(ctx, db, task, repo, "Pull request merge in "+member+" was refused because its target advanced"); err != nil {
				return err
			}
		}
	}
	return prBranchBehindFailure(task.Seq, task.GatedSHA, freshness, sync)
}

func (s *Service) requirePRTargetFresh(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, member, mergeMode, headSHA string) error {
	if !deliveredByPullRequest(mergeMode) || (!cfg.Defaults.RequireCurrentTarget && !greenhouseFullPipelineTaskWithConfig(task, cfg, member)) {
		return nil
	}
	targetBranch := taskTargetBranch(project, task)
	freshness, err := inspectPRTargetFreshness(ctx, project.Root, targetBranch, headSHA)
	if err != nil {
		return err
	}
	if freshness.Fresh {
		return nil
	}
	sync, err := s.queuePRTargetSyncInstruction(ctx, db, task, member, freshness, headSHA)
	if err != nil {
		return err
	}
	return prBranchBehindFailure(task.Seq, headSHA, freshness, sync)
}

func stalePRChecks() string {
	return marshalJSON(checkSnapshot{State: "STALE"})
}

// mergeQueueLabel summarizes a Task's queue places for Task lists, such as
// "2/3 main" or "api 1/2 main" for a Workspace Member.
func mergeQueueLabel(places []store.MergeQueuePlace) string {
	labels := make([]string, 0, len(places))
	for _, place := range places {
		label := fmt.Sprintf("%d/%d %s", place.Position, place.Length, place.TargetBranch)
		if place.Repo != "" {
			label = place.Repo + " " + label
		}
		labels = append(labels, label)
	}
	return strings.Join(labels, ", ")
}

func mergeQueueView(ctx context.Context, db *store.DB, task store.Task) ([]map[string]any, error) {
	places, err := db.MergeQueuePlaces(ctx, task.ProjectID)
	if err != nil {
		return nil, err
	}
	views := make([]map[string]any, 0, len(places[task.ID]))
	for _, place := range places[task.ID] {
		view := map[string]any{"target": place.TargetBranch, "position": place.Position, "length": place.Length, "head": taskIDString(place.HeadSeq)}
		if place.Repo != "" {
			view["repo"] = place.Repo
		}
		views = append(views, view)
	}
	return views, nil
}
