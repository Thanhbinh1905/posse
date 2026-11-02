package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// reviewPassNextStep builds the instruction a passing Review Round queues to
// a done full-pipeline Change Worker that requested it.
func reviewPassNextStep(ctx context.Context, db *store.DB, project store.Project, review store.Task) (*store.ReviewPassNextStep, error) {
	if review.Type != "review" || review.ReviewsTaskID < 1 {
		return nil, nil
	}
	change, err := db.TaskByID(ctx, project.ID, review.ReviewsTaskID)
	if err != nil {
		return nil, err
	}
	targets, err := db.ReviewTargets(ctx, review.ID)
	if err != nil {
		return nil, err
	}
	reason := fmt.Sprintf("Review Task %s passed on the private candidates of Task %s.", taskIDString(review.Seq), taskIDString(change.Seq))
	step, err := reviewNextStep(ctx, db, project, change, targets, reason)
	if step != nil {
		step.Summary = fmt.Sprintf("%s: Review Task %s passed; queued the Worker to validate and promote candidate %s", change.Title, taskIDString(review.Seq), strings.Join(stepHeads(step), ", "))
	}
	return step, err
}

// reviewNextStep is the one place that decides what a done full-pipeline
// Change Worker does once its reviewed heads are settled, whether by a pass or a
// disposition: validate and promote every reviewed private
// candidate. It returns nil when no reviewed candidate awaits validation; the
// store rechecks state when it queues the step.
func reviewNextStep(ctx context.Context, db *store.DB, project store.Project, change store.Task, targets []store.ReviewTarget, reason string) (*store.ReviewPassNextStep, error) {
	if change.Type != "change" || change.State != store.StateDone {
		return nil, nil
	}
	step := &store.ReviewPassNextStep{ChangeTaskID: change.ID}
	var checkouts []string
	var others []string
	for _, target := range targets {
		repo := ""
		if project.IsWorkspace() {
			repo = target.Repository
		}
		if !isFullPipelineTask(change, repo) {
			others = append(others, target.Repository)
			continue
		}
		candidate, err := db.LatestValidationCandidate(ctx, change.ID, repo)
		if store.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if candidate.State != store.ValidationCandidateActive || candidate.CandidateSHA != target.Commit {
			return nil, nil
		}
		step.Candidates = append(step.Candidates, store.ReviewPassCandidate{ID: candidate.ID, Repository: repo, SHA: candidate.CandidateSHA})
		checkouts = append(checkouts, candidate.WorktreePath)
	}
	if len(step.Candidates) == 0 {
		return nil, nil
	}
	var body strings.Builder
	fmt.Fprintf(&body, "%s Continue the full-pipeline sequence now; no further Review request is needed for these heads.\n", reason)
	heads := stepHeads(step)
	for i, candidate := range step.Candidates {
		repoFlag, label := "", "Private candidate"
		if candidate.Repository != "" {
			repoFlag = " --repo " + candidate.Repository
			label = "Member " + candidate.Repository + " private candidate"
		}
		fmt.Fprintf(&body, "\n%s %s, checkout `%s`. Run there, in order:\n", label, candidate.SHA, checkouts[i])
		fmt.Fprintf(&body, "1. `posse validate test%s --verify \"<command> -> <result>\" --proof \"<intent/E2E evidence>\"`\n", repoFlag)
		fmt.Fprintf(&body, "2. `posse validate docs%s --no-comments passed --proof \"<documentation review>\"` (add `--no-owner-doc-change \"<reason>\"` when no owner docs change)\n", repoFlag)
		fmt.Fprintf(&body, "3. `posse validate lint%s --proof \"<result>\" --coverage \"<workflow jobs and gaps>\"`\n", repoFlag)
		fmt.Fprintf(&body, "4. `posse publish%s \"<summary>\"` to promote it\n", repoFlag)
	}
	if len(others) > 0 {
		fmt.Fprintf(&body, "\nPublish the other changed Members (%s) through their own delivery mode.\n", strings.Join(others, ", "))
	}
	if project.IsWorkspace() {
		body.WriteString("\nAfter every changed Member is promoted or published, signal done without `--pr`.")
	} else if change.MergeMode == "pr" {
		body.WriteString("\nAfter promotion, signal done with the PR URL.")
	} else {
		body.WriteString("\nAfter promotion, signal done.")
	}
	body.WriteString(" A new candidate commit repeats Review -> test -> docs -> lint.")
	step.Body = body.String()
	step.Summary = fmt.Sprintf("%s: queued the Worker to validate and promote candidate %s", change.Title, strings.Join(heads, ", "))
	return step, nil
}

func stepHeads(step *store.ReviewPassNextStep) []string {
	heads := make([]string, len(step.Candidates))
	for i, candidate := range step.Candidates {
		heads[i] = shortSHA(candidate.SHA)
		if candidate.Repository != "" {
			heads[i] = candidate.Repository + "@" + heads[i]
		}
	}
	return heads
}

// queueSettledReviewStep queues the validate-and-promote step for a done
// full-pipeline Change Task whose current heads a disposition settled.
func queueSettledReviewStep(ctx context.Context, db *store.DB, project store.Project, change store.Task, reason string) error {
	targets, err := changeCandidateTargets(ctx, db, project, change)
	if err != nil {
		var failure *axi.Error
		if errors.As(err, &failure) {
			return nil
		}
		return err
	}
	step, err := reviewNextStep(ctx, db, project, change, targets, reason)
	if err != nil || step == nil {
		return err
	}
	step.Summary = fmt.Sprintf("%s: %s; queued the Worker to validate and promote candidate %s", change.Title, reason, strings.Join(stepHeads(step), ", "))
	return db.QueueReviewNextStep(ctx, project.ID, *step)
}

// settleReviewDisposition runs after `review dispose`: when the last round
// requested changes on the current heads and no blocking finding remains, the
// heads count as reviewed and the Worker gets the same next step as a pass.
func settleReviewDisposition(ctx context.Context, db *store.DB, project store.Project, change store.Task, action string) error {
	if change.Type != "change" || change.State != store.StateDone {
		return nil
	}
	plan, err := change.AdmittedPlan()
	if err != nil || plan == nil || plan.Review != "full-pipeline" {
		return nil
	}
	findings, err := db.ReviewFindingsForTask(ctx, change.ID)
	if err != nil {
		return err
	}
	for _, finding := range findings {
		if reviewFindingBlocks(finding, plan) {
			return nil
		}
	}
	targets, err := changeCandidateTargets(ctx, db, project, change)
	if err != nil {
		return nil
	}
	verdicts, err := db.ReviewVerdictsFor(ctx, change.ID)
	if err != nil || len(verdicts) == 0 {
		return err
	}
	last := verdicts[0]
	if last.Verdict != store.ReviewChangesRequested || !slices.Equal(last.Targets, targets) {
		return nil
	}
	if evidence, err := db.ReviewEvidenceFor(ctx, change.ID, targets); err != nil {
		return err
	} else if !evidence.Complete {
		heads := make([]string, len(targets))
		for i, target := range targets {
			heads[i] = target.Commit
		}
		if err := db.RecordReviewHeadDisposition(ctx, change.ID, heads, "no blocking finding remains after review dispose "+action); err != nil {
			return err
		}
	}
	return queueSettledReviewStep(ctx, db, project, change, "Review disposition "+action+" left no blocking finding on "+taskIDString(change.Seq))
}
