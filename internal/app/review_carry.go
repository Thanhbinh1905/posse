package app

import (
	"context"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// ensureReviewCarry records only the review component of evidence. It never
// carries test, docs, lint, config or forge results to a different head.
func ensureReviewCarry(ctx context.Context, db *store.DB, project store.Project, task store.Task, targets []store.ReviewTarget) error {
	if len(targets) != 1 {
		return nil
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	if plan == nil || (plan.Review != "quick" && plan.Review != "full-pipeline") {
		return nil
	}
	target := targets[0]
	root, targetSHA := task.WorktreePath, ""
	if fullPipelineSelected(task, "") {
		candidate, err := db.LatestValidationCandidate(ctx, task.ID, "")
		if err != nil || candidate.State != store.ValidationCandidateActive && candidate.State != store.ValidationCandidatePromoted {
			return nil
		}
		root, targetSHA = candidate.WorktreePath, candidate.TargetSHA
		remoteTarget, err := remoteBranchHead(ctx, root, "refs/heads/"+taskTargetBranch(project, task))
		if err != nil {
			return err
		}
		if remoteTarget != targetSHA {
			return nil
		}
	} else {
		var err error
		targetSHA, err = remoteBranchHead(ctx, root, "refs/heads/"+taskTargetBranch(project, task))
		if err != nil || targetSHA == "" {
			return nil
		}
		if _, err := gitFetch(ctx, root, "origin", taskTargetBranch(project, task)+":refs/remotes/origin/"+taskTargetBranch(project, task)); err != nil {
			return nil
		}
	}
	if _, err := gitOutput(ctx, root, "cat-file", "-e", targetSHA+"^{commit}"); err != nil {
		return nil
	}
	if _, err := gitOutput(ctx, root, "merge-base", "--is-ancestor", targetSHA, target.Commit); err != nil {
		return nil
	}
	qualifies, err := reviewVerdictQualifies(ctx, db, task)
	if err != nil {
		return err
	}
	verdicts, err := db.ReviewVerdictsFor(ctx, task.ID)
	if err != nil {
		return err
	}
	var passedHead string
	for _, verdict := range verdicts {
		if verdict.Verdict != store.ReviewPass || len(verdict.Targets) != 1 || verdict.Targets[0].Repository != target.Repository || !qualifies(verdict) {
			continue
		}
		passedHead = verdict.Targets[0].Commit
		descends, err := descendsThroughRewrites(ctx, db, root, task.ID, passedHead, target.Commit)
		if err != nil {
			return err
		}
		if descends {
			break
		}
		passedHead = ""
	}
	if passedHead == "" || passedHead == target.Commit {
		return nil
	}
	mergedTree, err := gitOutput(ctx, root, "merge-tree", "--write-tree", passedHead, targetSHA)
	if err != nil {
		return nil
	}
	fields := strings.Fields(mergedTree)
	if len(fields) == 0 {
		return nil
	}
	candidateTree, err := gitOutput(ctx, root, "rev-parse", target.Commit+"^{tree}")
	if err != nil || fields[0] != candidateTree {
		return nil
	}
	return db.RecordReviewCarry(ctx, store.ReviewCarry{
		ChangeTaskID: task.ID, Repository: target.Repository, FromHead: passedHead,
		ToHead: target.Commit, TargetCommitSHA: targetSHA, Tree: candidateTree,
	})
}

func (s *Service) reviewCarry(ctx *axi.Context, args []string) error {
	if len(args) != 3 {
		return axi.Usage("review carry requires a Change Task, the passed head and the current candidate head")
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
	change, err := s.currentTask(ctx.Context, db, project, args[0])
	if err != nil {
		return err
	}
	if !fullPipelineSelected(change, "") {
		return axi.Failure("review_carry_refused", "Review carries are restricted to private full-pipeline candidates", false)
	}
	candidate, err := db.LatestValidationCandidate(ctx.Context, change.ID, "")
	if err != nil || candidate.State != store.ValidationCandidateActive {
		return axi.Failure("candidate_missing", "Review carry requires an active private candidate", false)
	}
	if err := s.ensureCandidateCheckout(ctx.Context, db, home, change, candidate); err != nil {
		return err
	}
	candidate, err = db.LatestValidationCandidate(ctx.Context, change.ID, "")
	if err != nil {
		return err
	}
	if args[2] != candidate.CandidateSHA {
		return axi.Failure("review_carry_refused", "candidate-head must exactly match the active candidate SHA", false, candidate.CandidateSHA)
	}
	if len(args[1]) != 40 && len(args[1]) != 64 {
		return axi.Usage("passed-head must be a full commit SHA")
	}
	targets, err := changeCandidateTargets(ctx.Context, db, project, change)
	if err != nil {
		return err
	}
	if err := ensureReviewCarry(ctx.Context, db, project, change, targets); err != nil {
		return err
	}
	var carried bool
	if err := db.QueryRowContext(ctx.Context, `SELECT EXISTS(SELECT 1 FROM review_carries WHERE ship_task_id=? AND repository=? AND from_head=? AND to_head=?)`, change.ID, project.Name, args[1], candidate.CandidateSHA).Scan(&carried); err != nil {
		return err
	}
	if !carried {
		return axi.Failure("review_carry_refused", "candidate does not satisfy the clean passed-head/frozen-target tree proof", false)
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(change.Seq)}, {Key: "from_head", Value: args[1]}, {Key: "to_head", Value: candidate.CandidateSHA}, {Key: "target_head", Value: candidate.TargetSHA}, {Key: "evidence", Value: "review-only; test/docs/lint stages remain candidate-bound"}})
}
