package app

import (
	"context"
	"os/exec"
	"slices"
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
	sources, err := reviewCarrySources(ctx, db, task.ID, target.Repository)
	if err != nil {
		return err
	}
	if slices.Contains(sources, target.Commit) {
		return nil
	}
	candidateTree, err := gitOutput(ctx, root, "rev-parse", target.Commit+"^{tree}")
	if err != nil {
		return nil
	}
	for _, source := range sources {
		proved, err := reviewCarryProved(ctx, db, root, task.ID, source, targetSHA, target.Commit, candidateTree)
		if err != nil {
			return err
		}
		if proved {
			return db.RecordReviewCarry(ctx, store.ReviewCarry{
				ChangeTaskID: task.ID, Repository: target.Repository, FromHead: source,
				ToHead: target.Commit, TargetCommitSHA: targetSHA, Tree: candidateTree,
			})
		}
	}
	return nil
}

// reviewCarrySources lists the heads whose Review a target refresh may carry:
// single-repository passes, then exact heads the User accepted.
func reviewCarrySources(ctx context.Context, db *store.DB, taskID int64, repository string) ([]string, error) {
	verdicts, err := db.ReviewVerdictsFor(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var sources []string
	for _, verdict := range verdicts {
		if verdict.Verdict == store.ReviewPass && len(verdict.Targets) == 1 && verdict.Targets[0].Repository == repository {
			sources = append(sources, verdict.Targets[0].Commit)
		}
	}
	accepted, err := db.ReviewDispositionHeads(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return append(sources, accepted...), nil
}

// reviewCarryProved reports whether head is source plus a clean merge of
// targetSHA: either head descends from source and has the tree of that merge,
// or head's patch against targetSHA equals source's patch against its merge
// base with targetSHA.
func reviewCarryProved(ctx context.Context, db *store.DB, root string, taskID int64, source, targetSHA, head, headTree string) (bool, error) {
	merged, err := gitOutput(ctx, root, "merge-tree", "--write-tree", source, targetSHA)
	if err != nil {
		return false, nil
	}
	fields := strings.Fields(merged)
	if len(fields) == 0 {
		return false, nil
	}
	if fields[0] == headTree {
		descends, err := descendsThroughRewrites(ctx, db, root, taskID, source, head)
		if err != nil || descends {
			return descends, err
		}
	}
	sourcePatch, err := patchID(ctx, root, targetSHA+"..."+source)
	if err != nil || sourcePatch == "" {
		return false, nil
	}
	headPatch, err := patchID(ctx, root, targetSHA+"..."+head)
	return err == nil && headPatch == sourcePatch, nil
}

// patchID identifies the diff of a revision range by content, ignoring line
// numbers but not whitespace. An empty diff has no ID.
func patchID(ctx context.Context, root, revisions string) (string, error) {
	diff, err := gitOutputRaw(ctx, root, "diff", "--binary", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", revisions)
	if err != nil || diff == "" {
		return "", err
	}
	command := exec.CommandContext(ctx, "git", "-C", root, "patch-id", "--verbatim")
	command.Stdin = strings.NewReader(diff)
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(output)), " ")
	return id, nil
}

// reviewCarryNextStep names a carry to the active single-repository
// full-pipeline candidate and the validation that follows it.
func reviewCarryNextStep(ctx context.Context, db *store.DB, project store.Project, task store.Task, reopen bool) (axi.Object, error) {
	if project.IsWorkspace() || !fullPipelineSelected(task, "") {
		return nil, nil
	}
	candidate, err := db.LatestValidationCandidate(ctx, task.ID, "")
	if store.IsNotFound(err) || err == nil && candidate.State != store.ValidationCandidateActive {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	from, carried, err := db.ReviewCarrySource(ctx, task.ID, project.Name, candidate.CandidateSHA)
	if err != nil || !carried {
		return nil, err
	}
	step := "The Review of " + shortSHA(from) + " carries to candidate " + shortSHA(candidate.CandidateSHA) + " because it differs only by a clean merge of the target; no Review Round is due."
	if reopen {
		step += " Run `posse signal working`, then"
	} else {
		step += " Next,"
	}
	step += " in the candidate checkout run `posse validate test`, `posse validate docs` and `posse validate lint`, then `posse publish \"<summary>\"` to promote."
	return axi.Object{{Key: "review_carried_from", Value: from}, {Key: "next_step", Value: step}}, nil
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
