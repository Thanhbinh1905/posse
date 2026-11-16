package app

import (
	"context"
	"fmt"

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

func greenhouseWorkspacePendingHeads(ctx context.Context, db *store.DB, task store.Task) ([]greenhousePendingMember, error) {
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	var heads []greenhousePendingMember
	for _, repo := range repos {
		if repo.State == store.TaskRepoUnchanged || repo.State == store.TaskRepoMerged {
			continue
		}
		status, err := gitStatusOutput(ctx, repo.WorktreePath, "--porcelain", "--untracked-files=all")
		if err != nil {
			return nil, err
		}
		if status != "" {
			return nil, fmt.Errorf("member %s worktree is not clean", repo.Repo)
		}
		head, err := gitOutput(ctx, repo.WorktreePath, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
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
		passed, err := db.GreenhouseGatePass(ctx, task.ID, member.Repository, member.Head)
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
