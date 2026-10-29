package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

// reviewPassNextStep builds the instruction a passing Review Round queues to
// a done full-pipeline Change Worker that requested it: validate and promote
// every reviewed private candidate. It returns nil when no reviewed candidate
// awaits validation; the store rechecks state when it records the verdict.
func reviewPassNextStep(ctx context.Context, db *store.DB, project store.Project, review store.Task) (*store.ReviewPassNextStep, error) {
	if review.Type != "review" || review.ReviewsTaskID < 1 {
		return nil, nil
	}
	change, err := db.TaskByID(ctx, project.ID, review.ReviewsTaskID)
	if err != nil {
		return nil, err
	}
	if change.Type != "change" || change.State != store.StateDone {
		return nil, nil
	}
	targets, err := db.ReviewTargets(ctx, review.ID)
	if err != nil {
		return nil, err
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
	fmt.Fprintf(&body, "Review Task %s passed on the private candidates of Task %s. Continue the full-pipeline sequence now; no further Review request is needed for these heads.\n", taskIDString(review.Seq), taskIDString(change.Seq))
	heads := make([]string, len(step.Candidates))
	for i, candidate := range step.Candidates {
		repoFlag, label, head := "", "Private candidate", shortSHA(candidate.SHA)
		if candidate.Repository != "" {
			repoFlag = " --repo " + candidate.Repository
			label = "Member " + candidate.Repository + " private candidate"
			head = candidate.Repository + "@" + head
		}
		heads[i] = head
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
	step.Summary = fmt.Sprintf("%s: Review Task %s passed; queued the Worker to validate and promote candidate %s", change.Title, taskIDString(review.Seq), strings.Join(heads, ", "))
	return step, nil
}
