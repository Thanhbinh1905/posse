package app

import (
	"context"
	"fmt"
	"strings"

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

func prBranchBehindFailure(taskSeq int, headSHA string, freshness prTargetFreshness) error {
	message := fmt.Sprintf("pull request head %s does not contain current target commit %s from origin/%s", headSHA, freshness.Tip, freshness.Branch)
	return axi.Failure(
		"pr_branch_behind",
		message,
		false,
		"Merge origin/"+freshness.Branch+" into the Task branch, rerun the Gate, republish, and report done before retrying `posse merge "+taskIDString(taskSeq)+"`; never rebase a pushed branch",
	)
}

func workingPRBehindFailure(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	if task.State != store.StateWorking || task.Type != "change" || task.PRURL == "" || task.MergeMode != "pr" && task.MergeMode != "no-mistakes" {
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
	return prBranchBehindFailure(task.Seq, observation.HeadSHA, freshness)
}

func queuePRTargetSyncInstruction(ctx context.Context, db *store.DB, task store.Task, member, mergeMode string, freshness prTargetFreshness, headSHA string) error {
	if task.State != store.StateDone && task.State != store.StateMerging {
		return nil
	}
	memberLabel := ""
	if member != "" {
		memberLabel = "Workspace member " + member + ": "
	}
	instruction := fmt.Sprintf(
		"%sPR head %s is missing current target commit %s from origin/%s. Merge origin/%s into the Task branch (never rebase a pushed branch), resolve any conflicts, rerun the configured Gate, republish the updated head through this Task's delivery mode, and signal done again.",
		memberLabel, headSHA, freshness.Tip, freshness.Branch, freshness.Branch,
	)
	if mergeMode == "no-mistakes" {
		instruction = fmt.Sprintf(
			"%sPR head %s is missing current target commit %s from origin/%s. Merge origin/%s into the Task branch (never rebase a pushed branch), resolve any conflicts, then rerun no-mistakes with `--skip=rebase` and the Brief intent so its Gate and publication run on the updated head. Signal done again afterward.",
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
		return err
	}
	if len(signals) > 0 && signals[0].Verb == "done" {
		after = signals[0].At
	}
	dedupeKey := fmt.Sprintf("freshness:%s:%s:%s:%d", member, headSHA, freshness.Tip, after)
	_, err = db.QueueValidationFixInstruction(ctx, task.ID, "freshness", headSHA, dedupeKey, instruction)
	return err
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
	if err := queuePRTargetSyncInstruction(ctx, db, task, member, mergeMode, freshness, headSHA); err != nil {
		return err
	}
	return prBranchBehindFailure(task.Seq, headSHA, freshness)
}

func stalePRChecks() string {
	return marshalJSON(checkSnapshot{State: "STALE"})
}
