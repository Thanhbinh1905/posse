package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

type prTargetFreshness struct {
	Branch string
	Tip    string
	Fresh  bool
}

func inspectPRTargetFreshness(ctx context.Context, root, targetBranch, headSHA string) (prTargetFreshness, error) {
	freshness := prTargetFreshness{Branch: targetBranch}
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
	if strings.TrimSpace(headSHA) == "" {
		return freshness, fmt.Errorf("pull request has no head commit to check against origin/%s", targetBranch)
	}
	_, err = gitOutput(ctx, root, "merge-base", "--is-ancestor", targetRef, headSHA)
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
	if task.Type != "change" || task.MergeMode != "pr" && task.MergeMode != "no-mistakes" {
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

func workingPRBehindFailure(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	if task.State != store.StateWorking || task.Type != "change" || task.PRURL == "" || task.MergeMode != "pr" && task.MergeMode != "no-mistakes" {
		return nil
	}
	if err := pendingTargetSyncFailure(ctx, db, project, task); err != nil {
		return err
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
func queuePRTargetSyncInstruction(ctx context.Context, db *store.DB, task store.Task, member, mergeMode string, freshness prTargetFreshness, headSHA string) (targetSync, error) {
	if task.State != store.StateDone && task.State != store.StateMerging {
		return targetSync{}, nil
	}
	place, err := db.PlaceInMergeQueue(ctx, task.ID, member, freshness.Branch, time.Now().UnixMilli())
	if err != nil {
		return targetSync{}, err
	}
	if !place.Head() {
		return targetSync{Place: place}, nil
	}
	memberLabel := ""
	if member != "" {
		memberLabel = "Workspace member " + member + ": "
	}
	instruction := fmt.Sprintf(
		"%sPR head %s is missing current target commit %s from origin/%s. Merge origin/%s into the Task branch (never rebase a pushed branch), resolve any conflicts, republish the updated head through this Task's delivery mode, and signal done again.",
		memberLabel, headSHA, freshness.Tip, freshness.Branch, freshness.Branch,
	)
	if mergeMode == "no-mistakes" {
		instruction = fmt.Sprintf(
			"%sPR head %s is missing current target commit %s from origin/%s. Merge origin/%s into the Task branch (never rebase a pushed branch), resolve any conflicts, then rerun no-mistakes with `--skip=rebase` and the Brief intent. Signal done again afterward.",
			memberLabel, headSHA, freshness.Tip, freshness.Branch, freshness.Branch,
		)
	} else if member != "" {
		instruction += " For publication, use `posse publish --repo " + member + " \"<summary>\"` and report done without `--pr`."
	} else {
		instruction += " For publication, use `posse publish \"<summary>\"` and report done with the PR URL."
	}
	after := task.CreatedAt
	signals, err := db.TaskSignals(ctx, task.ID, 1)
	if err != nil {
		return targetSync{}, err
	}
	if len(signals) > 0 && signals[0].Verb == "done" {
		after = signals[0].At
	}
	dedupeKey := fmt.Sprintf("freshness:%s:%s:%s:%d", member, headSHA, freshness.Tip, after)
	if _, err = db.QueueValidationFixInstruction(ctx, task.ID, "freshness", headSHA, dedupeKey, instruction); err != nil {
		return targetSync{}, err
	}
	return targetSync{Queued: true, Place: place}, nil
}

func (s *Service) requirePRTargetFresh(ctx context.Context, db *store.DB, project store.Project, task store.Task, member, mergeMode, headSHA string) error {
	if mergeMode != "pr" && mergeMode != "no-mistakes" {
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
	sync, err := queuePRTargetSyncInstruction(ctx, db, task, member, mergeMode, freshness, headSHA)
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
