package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func taskUsesGreenhouseReviewProvider(task store.Task, cfg config.Config, repository string) bool {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return cfg.EffectiveReviewProvider(repository) == store.GreenhouseReviewProvider
	}
	if repository == "" && len(plan.Execution.Members) > 0 {
		for _, member := range plan.Execution.Members {
			provider := member.ReviewProvider
			if provider == "" {
				provider = plan.ReviewProvider
			}
			if provider == "" && plan.ValidationLoops != nil {
				provider = plan.ValidationLoops.ReviewProvider
			}
			if provider == "" {
				provider = cfg.EffectiveReviewProvider(member.Name)
			}
			if provider == store.GreenhouseReviewProvider {
				return true
			}
		}
		return false
	}
	if repository != "" {
		for _, member := range plan.Execution.Members {
			if member.Name == repository && member.ReviewProvider != "" {
				return member.ReviewProvider == store.GreenhouseReviewProvider
			}
		}
	}
	if plan.ReviewProvider != "" {
		return plan.ReviewProvider == store.GreenhouseReviewProvider
	}
	if plan.ValidationLoops != nil && plan.ValidationLoops.ReviewProvider != "" {
		return plan.ValidationLoops.ReviewProvider == store.GreenhouseReviewProvider
	}
	return cfg.EffectiveReviewProvider(repository) == store.GreenhouseReviewProvider
}

func greenhouseFullPipelineTaskWithConfig(task store.Task, cfg config.Config, repository string) bool {
	if repository != "" {
		return taskUsesGreenhouseReviewProvider(task, cfg, repository) && fullPipelineSelected(task, repository)
	}
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return false
	}
	if len(plan.Execution.Members) > 0 {
		return workspaceGreenhouseFullPipelinePlan(task, cfg)
	}
	return taskUsesGreenhouseReviewProvider(task, cfg, "") && fullPipelineSelected(task, "")
}

func fullPipelineTaskWithConfig(task store.Task, cfg config.Config, repository string) bool {
	return !taskUsesGreenhouseReviewProvider(task, cfg, repository) && fullPipelineSelected(task, repository)
}

type greenhousePendingMember struct {
	Repository string `json:"repository"`
	Head       string `json:"head"`
}

func pinGreenhouseReviewCheckout(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task, member greenhouseMember, head string) (greenhouseMember, error) {
	scratch, err := ensureTaskScratchForTask(ctx, db, home, project, task)
	if err != nil {
		return greenhouseMember{}, err
	}
	if member.name == "" || member.name == "." || member.name == ".." || filepath.Base(member.name) != member.name || strings.ContainsAny(member.name, `/\\`) {
		return greenhouseMember{}, fmt.Errorf("unsafe repository name %q for Greenhouse review checkout", member.name)
	}
	reviewRoot := filepath.Join(scratch, "greenhouse-review")
	if err := os.Mkdir(reviewRoot, 0o700); err != nil && !os.IsExist(err) {
		return greenhouseMember{}, err
	}
	info, err := os.Lstat(reviewRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = fmt.Errorf("greenhouse review checkout root is not a directory: %s", reviewRoot)
		}
		return greenhouseMember{}, err
	}
	parent := filepath.Join(reviewRoot, member.name)
	if err := os.Mkdir(parent, 0o700); err != nil && !os.IsExist(err) {
		return greenhouseMember{}, err
	}
	info, err = os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = fmt.Errorf("greenhouse review checkout directory is not a directory: %s", parent)
		}
		return greenhouseMember{}, err
	}
	checkout := filepath.Join(parent, "worktree")
	info, err = os.Lstat(checkout)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := gitOutput(ctx, member.root, "worktree", "add", "--quiet", "--detach", checkout, head); err != nil {
			return greenhouseMember{}, fmt.Errorf("create pinned Greenhouse review checkout: %w", err)
		}
	} else if err != nil {
		return greenhouseMember{}, err
	} else {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return greenhouseMember{}, fmt.Errorf("greenhouse review checkout is not a directory: %s", checkout)
		}
		checkoutRepository, err := gitCommonDir(ctx, checkout)
		if err != nil {
			return greenhouseMember{}, err
		}
		memberRepository, err := gitCommonDir(ctx, member.root)
		if err != nil {
			return greenhouseMember{}, err
		}
		if checkoutRepository != memberRepository {
			return greenhouseMember{}, fmt.Errorf("greenhouse review checkout belongs to another repository: %s", checkout)
		}
		status, err := gitStatusOutput(ctx, checkout, "--porcelain", "--untracked-files=all")
		if err != nil {
			return greenhouseMember{}, err
		}
		if status != "" {
			return greenhouseMember{}, fmt.Errorf("greenhouse review checkout is not clean: %s", checkout)
		}
		if _, err := gitOutput(ctx, checkout, "checkout", "--quiet", "--detach", head); err != nil {
			return greenhouseMember{}, fmt.Errorf("pin Greenhouse review checkout: %w", err)
		}
	}
	actual, err := gitOutput(ctx, checkout, "rev-parse", "HEAD")
	if err != nil || actual != head {
		if err == nil {
			err = fmt.Errorf("checkout resolved to %s, want %s", actual, head)
		}
		return greenhouseMember{}, err
	}
	member.worktree = checkout
	return member, nil
}

func greenhouseWorkspacePendingHeads(ctx context.Context, db *store.DB, project store.Project, task store.Task) ([]greenhousePendingMember, error) {
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	projectRepos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	roots := make(map[string]string, len(projectRepos))
	for _, repo := range projectRepos {
		roots[repo.Name] = filepath.Join(project.Root, repo.Path)
	}
	var heads []greenhousePendingMember
	for _, repo := range repos {
		if repo.State == store.TaskRepoUnchanged || repo.State == store.TaskRepoMerged {
			continue
		}
		root, ok := roots[repo.Repo]
		if !ok {
			return nil, fmt.Errorf("workspace member %s has no registered repository", repo.Repo)
		}
		head, err := lockedTaskBranchAuthorHead(ctx, root, task.Branch)
		if err != nil {
			return nil, fmt.Errorf("resolve Task branch for workspace member %s: %w", repo.Repo, err)
		}
		heads = append(heads, greenhousePendingMember{Repository: repo.Repo, Head: head})
	}
	if len(heads) == 0 {
		return nil, fmt.Errorf("workspace Task has no changed members")
	}
	return heads, nil
}

func workspaceGreenhouseHeadsNeedReview(ctx context.Context, db *store.DB, task store.Task, heads []greenhousePendingMember) (bool, error) {
	for _, member := range heads {
		passed, err := db.GreenhouseGatePass(ctx, task.ID, member.Repository, member.Head, greenhouseBlockAt(task))
		if err != nil {
			return false, err
		}
		if !passed {
			return true, nil
		}
	}
	return false, nil
}

func workspaceGreenhouseFullPipelinePlan(task store.Task, cfg config.Config) bool {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil || len(plan.Execution.Members) == 0 {
		return false
	}
	for _, member := range plan.Execution.Members {
		if !fullPipelineSelected(task, member.Name) || !taskUsesGreenhouseReviewProvider(task, cfg, member.Name) {
			return false
		}
	}
	return true
}

func workspaceGreenhouseFullPipeline(ctx context.Context, db *store.DB, task store.Task, cfg config.Config) (bool, error) {
	if task.Type != "change" {
		return false, nil
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return false, admittedPlanFailure(err)
	}
	if plan == nil {
		return false, nil
	}
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return false, err
	}
	changed := 0
	for _, repo := range repos {
		if repo.State == store.TaskRepoUnchanged || repo.State == store.TaskRepoMerged {
			continue
		}
		changed++
		if !fullPipelineSelected(task, repo.Repo) || !taskUsesGreenhouseReviewProvider(task, cfg, repo.Repo) {
			return false, nil
		}
	}
	return changed > 0, nil
}
