package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// abandonCandidate discards the newest unpromoted private candidate of a
// full-pipeline lane and returns the lane to the candidate promoted before
// it, whose head the Task branch must already be at. The abandoned head stays
// under its archive ref and the abandonment is recorded in Task history.
func (s *Service) abandonCandidate(out *axi.Context, args []string) (returnErr error) {
	parsed, err := parseArgs("candidate abandon", args, map[string]flagSpec{"repo": {}, "reason": {}})
	if err != nil {
		return err
	}
	reason := strings.TrimSpace(parsed.Flags["reason"])
	if len(parsed.Positionals) != 1 || reason == "" {
		return axi.Usage("candidate abandon requires one Task id and --reason <why>")
	}
	ctx := out.Context
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	var project store.Project
	var task store.Task
	source := "lead"
	if s.workerCaller(ctx, home) {
		source = "worker"
		task, err = s.workerTask(ctx, db)
		if err != nil {
			return axi.Failure("worker_task_unknown", "cannot find the Worker's Task", false)
		}
		if parsed.Positionals[0] != taskIDString(task.Seq) {
			return axi.Failure("candidate_abandon_refused", "a Worker can abandon only its own Task's candidate", false)
		}
		project, err = db.ProjectByID(ctx, task.ProjectID)
		if err != nil {
			return err
		}
	} else {
		project, err = s.projectForCWD(ctx, db)
		if err != nil {
			return err
		}
		if err := s.requireLead(ctx, db, project); err != nil {
			return err
		}
		task, err = s.currentTask(ctx, db, project, parsed.Positionals[0])
		if err != nil {
			return err
		}
	}
	repo := parsed.Flags["repo"]
	if task.Type != "change" || !isFullPipelineTask(task, repo) {
		return axi.Failure("validation_not_selected", "the admitted Task does not select full-pipeline validation", false)
	}
	lane, err := resolveCandidateLane(ctx, db, project, task, repo)
	if err != nil {
		return err
	}
	intent, err := s.startTaskIntent(ctx, db, project.ID, task.ID, "candidate abandon")
	if err != nil {
		return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	defer func() {
		if finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID); returnErr == nil && finishErr != nil {
			returnErr = finishErr
		}
	}()

	candidate, err := db.LatestValidationCandidate(ctx, task.ID, repo)
	if err != nil {
		if store.IsNotFound(err) {
			return axi.Failure("candidate_missing", lane.memberLabel("no private candidate has been submitted"), false)
		}
		return err
	}
	switch candidate.State {
	case store.ValidationCandidatePreparing, store.ValidationCandidateActive, store.ValidationCandidateStale:
	case store.ValidationCandidatePromoting:
		return axi.Failure("candidate_promotion_in_progress", lane.memberLabel("candidate "+shortSHA(candidate.CandidateSHA)+" is being promoted"), true, "Resume its promotion with `posse publish` instead")
	default:
		return axi.Failure("candidate_not_pending", lane.memberLabel("no unpromoted candidate is pending; the latest is "+candidate.State+" at "+shortSHA(candidate.CandidateSHA)), false)
	}
	promoted, err := db.PromotedValidationCandidateBefore(ctx, task.ID, repo, candidate.Generation)
	if err != nil {
		if store.IsNotFound(err) {
			return axi.Failure("candidate_abandon_refused", lane.memberLabel("no promoted candidate precedes candidate "+shortSHA(candidate.CandidateSHA)+" to return to"), false)
		}
		return err
	}
	head, branch, err := authorBranchHead(ctx, lane.task)
	if err != nil {
		return err
	}
	if branch != "refs/heads/"+task.Branch || head != promoted.PromotedSHA {
		return axi.Failure("candidate_abandon_refused", lane.memberLabel(fmt.Sprintf("Task branch is at %s (checkout %s), not the promoted head %s", shortSHA(head), branch, shortSHA(promoted.PromotedSHA))), false,
			fmt.Sprintf("Reset the Task branch in the Sandbox to %s, for example with `git reset --hard %s`, then retry", promoted.PromotedSHA, promoted.PromotedSHA))
	}
	if remoteHead, err := lane.remoteTaskHead(ctx); err != nil {
		return axi.Failure("candidate_remote_head_failed", lane.memberLabel("could not inspect the Task branch on origin"), true, err.Error())
	} else if !lane.localDelivery() && remoteHead != promoted.PromotedSHA {
		return axi.Failure("candidate_abandon_refused", lane.memberLabel("origin Task branch is at "+shortSHA(remoteHead)+", not the promoted head "+shortSHA(promoted.PromotedSHA)), false)
	}

	if candidate.State != store.ValidationCandidateStale {
		// Record commits made in the candidate checkout so its archive keeps them.
		if err := s.ensureCandidateCheckout(ctx, db, home, lane.task, candidate); err != nil {
			return err
		}
		if candidate, err = db.LatestValidationCandidate(ctx, task.ID, repo); err != nil {
			return err
		}
		if err := db.MarkValidationCandidateStale(ctx, candidate.ID, head); err != nil {
			return err
		}
		candidate.State = store.ValidationCandidateStale
	}
	archiveRef, archivedHead, err := archiveCandidateRef(ctx, db, lane.task, candidate)
	if err != nil {
		return err
	}
	note := lane.memberLabel(fmt.Sprintf("Abandoned candidate %s (generation %d) and returned to promoted head %s (generation %d): %s", shortSHA(archivedHead), candidate.Generation, shortSHA(promoted.PromotedSHA), promoted.Generation, reason))
	if err := db.AbandonValidationCandidate(ctx, candidate, archiveRef, archivedHead, promoted, candidateRefName(lane.task), source, note); err != nil {
		return err
	}
	if promoted, err = db.LatestValidationCandidate(ctx, task.ID, repo); err != nil {
		return err
	}
	if err := s.ensureCandidateCheckout(ctx, db, home, lane.task, promoted); err != nil {
		return err
	}
	result := axi.Object{{Key: "task", Value: taskIDString(task.Seq)}}
	if repo != "" {
		result = append(result, axi.Field{Key: "repo", Value: repo})
	}
	return out.Print(append(result,
		axi.Field{Key: "abandoned_head", Value: archivedHead}, axi.Field{Key: "abandoned_ref", Value: archiveRef},
		axi.Field{Key: "promoted_head", Value: promoted.PromotedSHA},
		axi.Field{Key: "result", Value: note},
		axi.Field{Key: "help", Value: []any{"Run `posse signal done \"<summary>\"` from the Task Sandbox; the promoted head needs no republish"}},
	))
}

// pendingCandidateFailure refuses Merge and withholds merge_ready while a lane
// has a candidate that was neither promoted nor abandoned, since the head
// Merge would deliver is no longer the Task's latest submission.
func pendingCandidateFailure(ctx context.Context, db *store.DB, task store.Task) error {
	candidates, err := db.LiveValidationCandidates(ctx, task.ID)
	if err != nil || len(candidates) == 0 {
		return err
	}
	candidate := candidates[0]
	delivered := task.GatedSHA
	if promoted, err := db.PromotedValidationCandidateBefore(ctx, task.ID, candidate.Repo, candidate.Generation); err == nil {
		delivered = promoted.PromotedSHA
	} else if !store.IsNotFound(err) {
		return err
	}
	message := "newer candidate " + shortSHA(candidate.SubmittedSHA) + " is pending and not promoted"
	if delivered != "" {
		message += "; Merge would deliver " + shortSHA(delivered)
	}
	command := "posse candidate abandon " + taskIDString(task.Seq)
	if candidate.Repo != "" {
		message = candidate.Repo + ": " + message
		command += " --repo " + candidate.Repo
	}
	return axi.Failure("candidate_pending", message, false, "Promote it through validation with `posse publish`, or abandon it with `"+command+" --reason \"<why>\"`")
}
