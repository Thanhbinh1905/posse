package app

import (
	"fmt"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// promotePrivateCandidate is an internal VP-C seam, not a production command.
// Its caller must establish all frozen local validation evidence before entry.
func (s *Service) promotePrivateCandidate(out *axi.Context, db *store.DB, home string, project store.Project, task store.Task, repoName string, cfg config.Config) (returnErr error) {
	ctx := out.Context
	candidate, err := db.LatestValidationCandidate(ctx, task.ID, repoName)
	if err != nil {
		if store.IsNotFound(err) {
			return axi.Failure("candidate_missing", "no private candidate has been submitted for this Task", false)
		}
		return err
	}
	if candidate.State == store.ValidationCandidatePromoted {
		return printPromotedCandidate(out, db, task, candidate, repoName)
	}
	if candidate.State != store.ValidationCandidateActive && candidate.State != store.ValidationCandidatePromoting {
		return candidateSubmissionError(task, candidate, "candidate_not_promotable", "candidate is not active", false)
	}
	if task.State != store.StateWorking && task.State != store.StateStalled && task.State != store.StateDone {
		return axi.Failure("candidate_promote_refused", "Task cannot promote a candidate in its current state", false, string(task.State))
	}
	if err := validateChangeSignal(ctx, task); err != nil {
		// A promoted-but-not-pushed recovery may already have advanced the
		// Task branch. It is checked exactly below instead of as an author edit.
		if candidate.State != store.ValidationCandidatePromoting {
			return axi.Failure("candidate_promote_refused", err.Error(), false)
		}
	}
	intent, err := s.startTaskIntent(ctx, db, project.ID, task.ID, "publish")
	if err != nil {
		return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	defer func() {
		if finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID); returnErr == nil && finishErr != nil {
			returnErr = finishErr
		}
	}()
	if err := s.ensureCandidateCheckout(ctx, db, home, task, candidate); err != nil && candidate.State != store.ValidationCandidatePromoting {
		return err
	}
	candidate, err = db.LatestValidationCandidate(ctx, task.ID, repoName)
	if err != nil {
		return err
	}
	candidateHead, found, err := readGitRef(ctx, task.WorktreePath, candidate.RefName)
	if err != nil {
		return err
	}
	if !found {
		return axi.Failure("candidate_ref_missing", "private candidate ref is missing", false, candidate.RefName)
	}
	if candidateHead != candidate.CandidateSHA {
		if !gitAncestor(ctx, task.WorktreePath, candidate.CandidateSHA, candidateHead) {
			return axi.Failure("candidate_ref_moved", "private candidate ref does not descend from its recorded head", false)
		}
		if err := db.AdvanceValidationCandidate(ctx, candidate.ID, candidate.CandidateSHA, candidateHead); err != nil {
			return err
		}
		candidate.CandidateSHA = candidateHead
	}
	if !gitAncestor(ctx, task.WorktreePath, candidate.SubmittedSHA, candidate.CandidateSHA) {
		return axi.Failure("candidate_not_fast_forward", "candidate is not a fast-forward of the submitted author head", false)
	}
	remoteHead, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+task.Branch)
	if err != nil {
		return axi.Failure("candidate_remote_head_failed", "could not inspect the Task branch before promotion", true, err.Error())
	}
	alreadyPushed := candidate.State == store.ValidationCandidatePromoting && remoteHead == candidate.CandidateSHA
	if !alreadyPushed && remoteHead != candidate.ExpectedRemoteSHA {
		invalidator := remoteHead
		if invalidator == "" {
			invalidator = candidate.CandidateSHA
		}
		if err := db.MarkValidationCandidateStale(ctx, candidate.ID, invalidator); err != nil {
			return err
		}
		return candidateSubmissionError(task, candidate, "candidate_remote_moved", "origin Task branch moved after candidate submission", false)
	}
	if alreadyPushed {
		if err := requireFullPipelineCandidateResults(ctx, db, project, task, candidate); err != nil {
			return err
		}
	} else if err := requireFullPipelineCandidateValidation(ctx, db, project, task, candidate); err != nil {
		return err
	}
	candidateStatus, err := gitStatusOutput(ctx, candidate.WorktreePath, "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if candidateStatus != "" {
		return axi.Failure("candidate_dirty", "private candidate checkout changed after its validation results were recorded", false, "Commit the candidate changes and rerun review -> test -> docs -> lint")
	}

	actualAuthor, authorBranch, err := authorBranchHead(ctx, task)
	if err != nil {
		return err
	}
	authorOnExpectedHead := candidate.State == store.ValidationCandidateActive && actualAuthor == candidate.SubmittedSHA || candidate.State == store.ValidationCandidatePromoting && actualAuthor == candidate.CandidateSHA
	authorAdvancedAfterPush := alreadyPushed && gitAncestor(ctx, task.WorktreePath, candidate.CandidateSHA, actualAuthor)
	if authorBranch != "refs/heads/"+task.Branch || !authorOnExpectedHead && !authorAdvancedAfterPush {
		if candidate.State == store.ValidationCandidateActive && actualAuthor != candidate.SubmittedSHA || candidate.State == store.ValidationCandidatePromoting && !alreadyPushed {
			_ = db.MarkValidationCandidateStale(ctx, candidate.ID, actualAuthor)
		}
		return candidateAuthorMovedFailure(task, candidate, actualAuthor, authorBranch)
	}
	if !alreadyPushed {
		targetHead, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+taskTargetBranch(project, task))
		if err != nil {
			return axi.Failure("candidate_target_head_failed", "could not recheck the frozen target before promotion", true, err.Error())
		}
		if targetHead == "" {
			return axi.Failure("candidate_target_missing", "the frozen target branch is missing from origin", false)
		}
		if targetHead != candidate.TargetSHA {
			if err := db.MarkValidationCandidateStale(ctx, candidate.ID, targetHead); err != nil {
				return err
			}
			return axi.Failure("candidate_target_moved", "the frozen target moved after candidate submission", false, "Refresh candidate freshness and rerun validation before promotion")
		}
		if remoteHead != candidate.ExpectedRemoteSHA {
			invalidator := remoteHead
			if invalidator == "" {
				invalidator = candidate.CandidateSHA
			}
			if err := db.MarkValidationCandidateStale(ctx, candidate.ID, invalidator); err != nil {
				return err
			}
			return axi.Failure("candidate_remote_moved", "origin Task branch moved after candidate submission", false, fmt.Sprintf("expected %s, found %s", shortSHA(candidate.ExpectedRemoteSHA), shortSHA(remoteHead)))
		}
	}
	status, err := gitStatusOutput(ctx, task.WorktreePath, "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return axi.Failure("candidate_author_dirty", "the Worker's Sandbox is not clean; promotion will not overwrite it", false, "Commit or preserve Sandbox changes, then retry promotion")
	}
	if candidate.State == store.ValidationCandidateActive {
		if err := s.runIntentStep(ctx, db, intent, "candidate.promoting", func() error {
			return db.BeginValidationCandidatePromotion(ctx, candidate.ID)
		}); err != nil {
			return err
		}
		candidate.State = store.ValidationCandidatePromoting
	}
	if !alreadyPushed {
		if actualAuthor == candidate.SubmittedSHA {
			if err := s.runIntentStep(ctx, db, intent, "candidate.branch", func() error {
				refHead, found, err := readGitRef(ctx, task.WorktreePath, candidate.RefName)
				if err != nil || !found || refHead != candidate.CandidateSHA {
					return axi.Failure("candidate_ref_moved", "private candidate ref changed before fast-forward", false, candidate.RefName)
				}
				current, branch, err := authorBranchHead(ctx, task)
				if err != nil {
					return err
				}
				if branch != "refs/heads/"+task.Branch || current != candidate.SubmittedSHA {
					return axi.Failure("candidate_author_moved", "Task branch changed before candidate fast-forward", false, shortSHA(candidate.SubmittedSHA), shortSHA(current))
				}
				if _, err := gitOutput(ctx, task.WorktreePath, "merge", "--ff-only", "--no-edit", candidate.CandidateSHA); err != nil {
					return axi.Failure("candidate_not_fast_forward", "Task branch could not fast-forward to the candidate", false, err.Error())
				}
				return nil
			}); err != nil {
				return err
			}
		}
		current, branch, err := authorBranchHead(ctx, task)
		if err != nil {
			return err
		}
		if branch != "refs/heads/"+task.Branch || current != candidate.CandidateSHA {
			return axi.Failure("candidate_author_moved", "Task branch did not fast-forward to the candidate", false, shortSHA(candidate.CandidateSHA), shortSHA(current))
		}
		remoteHead, err = remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+task.Branch)
		if err != nil {
			return axi.Failure("candidate_remote_head_failed", "could not recheck the Task branch on origin", true, err.Error())
		}
		if remoteHead != candidate.ExpectedRemoteSHA {
			return axi.Failure("candidate_remote_moved", "origin Task branch moved before promotion", false, fmt.Sprintf("expected %s, found %s", shortSHA(candidate.ExpectedRemoteSHA), shortSHA(remoteHead)))
		}
		candidateRefHead, candidateRefFound, err := readGitRef(ctx, task.WorktreePath, candidate.RefName)
		if err != nil || !candidateRefFound || candidateRefHead != candidate.CandidateSHA {
			return axi.Failure("candidate_ref_moved", "private candidate ref changed before push", false, candidate.RefName)
		}
		targetHead, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+taskTargetBranch(project, task))
		if err != nil {
			return axi.Failure("candidate_target_head_failed", "could not recheck the frozen target immediately before promotion", true, err.Error())
		}
		if targetHead == "" {
			return axi.Failure("candidate_target_missing", "the frozen target branch is missing from origin", false)
		}
		if targetHead != candidate.TargetSHA {
			if err := db.MarkValidationCandidateStale(ctx, candidate.ID, targetHead); err != nil {
				return err
			}
			return axi.Failure("candidate_target_moved", "the frozen target moved before promotion", false, "Refresh candidate freshness and rerun validation before promotion")
		}
		if err := db.RecordPublishPrePushHead(ctx, task.ID, repoName, candidate.ExpectedRemoteSHA); err != nil {
			return err
		}
		if err := s.runIntentStep(ctx, db, intent, "candidate.push", func() error {
			_, err := gitOutput(ctx, task.WorktreePath, "push", "origin", candidate.CandidateSHA+":refs/heads/"+task.Branch)
			if err != nil {
				return axi.Failure("pr_push_failed", "could not fast-forward the Task branch on origin", true, err.Error())
			}
			return nil
		}); err != nil {
			return err
		}
		remoteHead, err = remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+task.Branch)
		if err != nil || remoteHead != candidate.CandidateSHA {
			return axi.Failure("candidate_remote_mismatch", "origin does not contain the promoted candidate head", true, shortSHA(candidate.CandidateSHA), shortSHA(remoteHead))
		}
	}

	forge, err := forgeForRepository(ctx, project.Root, cfg, repoName)
	if err != nil {
		return err
	}
	if err := validateForgeMergeMethod(forge, cfg); err != nil {
		return err
	}
	promotionTask := task
	promotionTask.WorktreePath = candidate.WorktreePath
	promotionTask.GatedSHA = candidate.CandidateSHA
	prURL, _, err := s.findOrCreatePullRequest(ctx, db, project, promotionTask, intent, forge, repoName, candidate.Summary, candidate.Verification, candidate.Proof, candidate.Risk, candidate.Refresh, candidate.ExpectedRemoteSHA)
	if err != nil {
		return err
	}
	if err := validateWorkerPullRequest(ctx, project, promotionTask, forge, prURL, candidate.CandidateSHA); err != nil {
		return err
	}
	if repoName == "" {
		if err := db.RecordVerifiedPRHead(ctx, task.ID, prURL, candidate.CandidateSHA); err != nil {
			return err
		}
		if err := createPROpenedNotice(ctx, db, project, task, prURL, candidate.CandidateSHA, task.Title+": pull request opened"); err != nil {
			return err
		}
	} else {
		repos, err := db.TaskRepos(ctx, task.ID)
		if err != nil {
			return err
		}
		for _, repo := range repos {
			if repo.Repo == repoName {
				repo.PRURL = prURL
				if err := db.UpdateTaskRepo(ctx, repo); err != nil {
					return err
				}
				break
			}
		}
	}
	if err := db.CompleteValidationCandidatePromotion(ctx, candidate.ID, candidate.CandidateSHA); err != nil {
		return err
	}
	return out.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)}, {Key: "candidate_head", Value: candidate.CandidateSHA},
		{Key: "pr_url", Value: prURL}, {Key: "result", Value: fmt.Sprintf("promoted: candidate %s at %s", taskIDString(task.Seq), shortSHA(candidate.CandidateSHA))},
	})
}
