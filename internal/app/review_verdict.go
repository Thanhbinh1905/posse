package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// shipCandidateTargets resolves the commits a Ship Task currently offers for
// landing: its branch tip in the Project repository, or in each changed Member
// of a workspace.
func shipCandidateTargets(ctx context.Context, db *store.DB, project store.Project, ship store.Task) ([]store.ReviewTarget, error) {
	if ship.Branch == "" {
		return nil, fmt.Errorf("task %s has no branch", taskIDString(ship.Seq))
	}
	ref := "refs/heads/" + ship.Branch
	if !project.IsWorkspace() {
		sha, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return nil, err
		}
		return []store.ReviewTarget{{Repository: project.Name, Commit: sha}}, nil
	}
	members, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	for _, member := range members {
		paths[member.Name] = filepath.Join(project.Root, member.Path)
	}
	repos, err := db.TaskRepos(ctx, ship.ID)
	if err != nil {
		return nil, err
	}
	var targets []store.ReviewTarget
	for _, repo := range repos {
		if repo.State == store.TaskRepoUnchanged {
			continue
		}
		root, found := paths[repo.Repo]
		if !found {
			return nil, fmt.Errorf("member %s of task %s is not in the workspace", repo.Repo, taskIDString(ship.Seq))
		}
		sha, err := gitOutput(ctx, root, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return nil, err
		}
		targets = append(targets, store.ReviewTarget{Repository: repo.Repo, Commit: sha})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("task %s has no changed repository", taskIDString(ship.Seq))
	}
	return targets, nil
}

// reviewableShip refuses a Ship Task that has not completed. Only a completed
// candidate has a revision that can be pinned.
func reviewableShip(ship store.Task) error {
	if ship.State != store.StateDone && ship.State != store.StateLanding {
		return axi.Failure("review_target_incomplete", fmt.Sprintf("Ship Task %s is %s; a Review Task targets a completed Ship revision", taskIDString(ship.Seq), ship.State), false, "Wait until the Ship Task is done, then dispatch the Review")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

// verifyReviewerSawTargets checks that the Reviewer's own checkout contains
// every bound commit, so a verdict cannot be recorded for a revision the
// Reviewer's worktree never held.
func verifyReviewerSawTargets(ctx context.Context, db *store.DB, project store.Project, review store.Task, targets []store.ReviewTarget) error {
	roots := map[string]string{project.Name: review.WorktreePath}
	if project.IsWorkspace() {
		repos, err := db.TaskRepos(ctx, review.ID)
		if err != nil {
			return err
		}
		roots = map[string]string{}
		for _, repo := range repos {
			roots[repo.Repo] = repo.WorktreePath
		}
	}
	for _, target := range targets {
		root := roots[target.Repository]
		if root == "" {
			return fmt.Errorf("the Review worktree has no checkout of %s", target.Repository)
		}
		if _, err := gitOutput(ctx, root, "merge-base", "--is-ancestor", target.Commit, "HEAD"); err != nil {
			return fmt.Errorf("the Review worktree does not contain the bound commit %s of %s", shortSHA(target.Commit), target.Repository)
		}
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// reviewVerdictQualifies applies the Reviewer identity rule to a recorded
// verdict: the Reviewer launch that produced it must still differ from every
// author launch of the Ship Task.
func reviewVerdictQualifies(ctx context.Context, db *store.DB, ship store.Task) (func(store.ReviewVerdict) bool, error) {
	launches, err := db.TaskLaunchIdentities(ctx, ship.ID)
	if err != nil {
		return nil, err
	}
	known := ship.Launches > 0 && launchHistoryComplete(ship, launches)
	return func(verdict store.ReviewVerdict) bool {
		return compareReviewIdentity(launches, known, verdict.Reviewer).Eligible
	}, nil
}

// currentReviewPass reports the pass verdict, if any, that qualifies the Ship
// Task's present candidate for local review.
func currentReviewPass(ctx context.Context, db *store.DB, project store.Project, ship store.Task) (store.ReviewVerdict, bool, error) {
	candidate, err := shipCandidateTargets(ctx, db, project, ship)
	if err != nil {
		// A candidate that cannot be resolved has no current pass.
		return store.ReviewVerdict{}, false, nil
	}
	return reviewPassFor(ctx, db, ship, candidate)
}

func reviewPassFor(ctx context.Context, db *store.DB, ship store.Task, candidate []store.ReviewTarget) (store.ReviewVerdict, bool, error) {
	qualifies, err := reviewVerdictQualifies(ctx, db, ship)
	if err != nil {
		return store.ReviewVerdict{}, false, err
	}
	return db.CurrentReviewPass(ctx, ship.ID, candidate, qualifies)
}

// requireReviewPass requires a D01 pass of the exact candidate: commit when
// given, otherwise the current Task branch tips.
func requireReviewPass(ctx context.Context, db *store.DB, project store.Project, ship store.Task, commit string) error {
	var ok bool
	var err error
	if commit == "" {
		_, ok, err = currentReviewPass(ctx, db, project, ship)
	} else {
		_, ok, err = reviewPassFor(ctx, db, ship, []store.ReviewTarget{{Repository: project.Name, Commit: commit}})
	}
	if err != nil {
		return err
	}
	if !ok {
		id := taskIDString(ship.Seq)
		revision := "its current revision"
		if commit != "" {
			revision = "candidate " + shortSHA(commit)
		}
		return axi.Failure("review_required", "Task "+id+" has no passing Review of "+revision, false, "Dispatch a Review Task for "+id+" with `review_of: "+id+"` and wait for a `pass` verdict; only an explicit User choice may select lighter review")
	}
	return nil
}

// requireSelectedReview keeps evidence separate from disposition and merge
// grants. Version-1 plans retain their frozen pre-policy behavior, visibly legacy.
func requireSelectedReview(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task) error {
	return requireSelectedReviewAt(ctx, db, project, cfg, task, "")
}

// requireSelectedReviewAt binds the selected evidence to commit, the exact
// pull request head a Posse merge will land. An empty commit uses the Task
// branch tip, which is the candidate of local integration.
func requireSelectedReviewAt(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, commit string) error {
	policies, err := selectedReviewPolicies(ctx, db, project, cfg, task)
	if err != nil {
		return err
	}
	local, receipt := false, false
	for _, selected := range policies {
		switch selected {
		case "none":
		case "local", "always":
			local = true
		case "no-mistakes":
			receipt = true
		default:
			return axi.Failure("review_policy_invalid", "unsupported selected review policy "+selected, false)
		}
	}
	if receipt {
		if project.IsWorkspace() {
			return axi.Failure("review_receipt_unsupported", "Task "+taskIDString(task.Seq)+" selects no-mistakes review for a workspace Member; no-mistakes cannot produce Member receipts", false, "Select local or none review for workspace Members")
		}
		if err := requireNoMistakesReceipt(ctx, db, project, task, commit); err != nil {
			return err
		}
	}
	if local {
		return requireReviewPass(ctx, db, project, task, commit)
	}
	return nil
}

// selectedReviewPolicies lists the admitted policy of the Task, or of each
// changed workspace Member. A version-1 plan keeps its frozen legacy value:
// only `always` required a pass, so the other legacy values select none.
func selectedReviewPolicies(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task) ([]string, error) {
	plan, err := task.AdmittedPlan()
	if err != nil {
		return nil, admittedPlanFailure(err)
	}
	policy := cfg.Defaults.Review
	if plan != nil {
		policy = plan.Review
		if plan.Version == 1 {
			if policy == "always" {
				return []string{"always"}, nil
			}
			return nil, nil // Frozen legacy behavior, not an inferred evidence guarantee.
		}
	}
	if !project.IsWorkspace() {
		return []string{policy}, nil
	}
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	var policies []string
	for _, repo := range repos {
		if repo.State == store.TaskRepoLanded {
			continue
		}
		ahead, err := commitsAhead(ctx, repo.WorktreePath, repo.BaseRef, "refs/heads/"+task.Branch)
		if err != nil {
			return nil, memberFailure(repo.Repo, err)
		}
		if ahead == 0 {
			continue
		}
		selected := cfg.MemberReview(repo.Repo)
		if plan != nil {
			selected = ""
			for _, member := range plan.Execution.Members {
				if member.Name == repo.Repo {
					selected = member.Review
					break
				}
			}
		}
		policies = append(policies, selected)
	}
	return policies, nil
}

func reviewEvidenceStatus(task store.Task) string {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || plan.Version == 1 {
		return "legacy-evidence"
	}
	return "selected-policy"
}

func reviewVerdictView(verdict store.ReviewVerdict, current bool) map[string]any {
	targets := make([]string, 0, len(verdict.Targets))
	for _, target := range verdict.Targets {
		targets = append(targets, target.Repository+"@"+target.Commit)
	}
	return map[string]any{
		"verdict": verdict.Verdict, "current": current, "targets": strings.Join(targets, ","), "report_sha256": verdict.ReportSHA256,
		"reviewer_launch": verdict.Reviewer.LaunchNumber, "reviewer_profile": verdict.Reviewer.Profile, "reviewer_model": verdict.Reviewer.ConfiguredModel,
	}
}
