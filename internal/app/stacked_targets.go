package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

type blockedStackTargetChange struct{ reason string }

type skippedStackTargetChange struct{ reason string }

func (e blockedStackTargetChange) Error() string { return e.reason }
func (e skippedStackTargetChange) Error() string { return e.reason }

func (s *Service) enqueueStackedTargetChanges(ctx context.Context, db *store.DB, project store.Project, lower store.Task, repo, lowerTarget string) error {
	if lower.Branch == "" || lowerTarget == "" {
		return nil
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, upper := range tasks {
		if upper.ID == lower.ID || !strings.EqualFold(upper.Type, "ship") || upper.State == store.StateLanded || upper.State == store.StateTornDown || upper.State == store.StateReported {
			continue
		}
		if !project.IsWorkspace() {
			if upper.TargetBranch != lower.Branch {
				continue
			}
			if _, err := db.BeginTaskTargetChange(ctx, project.ID, upper.ID, "", lower.ID, lower.Branch, lowerTarget); err != nil {
				return err
			}
			continue
		}
		repos, err := db.TaskRepos(ctx, upper.ID)
		if err != nil {
			return err
		}
		for _, member := range repos {
			if member.Repo != repo || member.State == store.TaskRepoLanded || member.TargetBranch != lower.Branch {
				continue
			}
			if _, err := db.BeginTaskTargetChange(ctx, project.ID, upper.ID, repo, lower.ID, lower.Branch, lowerTarget); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) processPendingStackedTargetChanges(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, repoFilter string) error {
	changes, err := db.PendingTaskTargetChanges(ctx, project.ID)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, change := range changes {
		if repoFilter != "" && change.Repo != repoFilter {
			continue
		}
		upper, err := db.TaskByID(ctx, project.ID, change.TaskID)
		if err != nil {
			return err
		}
		blocked, processErr := processStackedTargetChange(ctx, db, project, cfg, upper, change)
		if blocked != nil {
			if skipped, ok := blocked.(skippedStackTargetChange); ok {
				if skipErr := db.SkipTaskTargetChange(ctx, change.ID, skipped); skipErr != nil {
					return skipErr
				}
				continue
			}
			if blockErr := db.BlockTaskTargetChange(ctx, change.ID, blocked); blockErr != nil {
				return blockErr
			}
			if noticeErr := recordStackedTargetChangeFailure(ctx, db, project, upper, change, blocked, now); noticeErr != nil {
				return noticeErr
			}
			continue
		}
		if processErr != nil {
			if noticeErr := recordStackedTargetChangeFailure(ctx, db, project, upper, change, processErr, now); noticeErr != nil {
				return noticeErr
			}
		}
	}
	return nil
}

func processStackedTargetChange(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, upper store.Task, change store.TaskTargetChange) (error, error) {
	if change.PRURL == "" {
		return nil, db.CompleteTaskTargetChange(ctx, change.ID)
	}
	root := project.Root
	member := ""
	if change.Repo != "" {
		repos, err := db.ProjectRepos(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		found := false
		for _, repo := range repos {
			if repo.Name == change.Repo && repo.Status == store.RepoActive {
				root = filepath.Join(project.Root, repo.Path)
				member = repo.Name
				found = true
				break
			}
		}
		if !found {
			return blockedStackTargetChange{reason: "stacked target member repository is unavailable"}, nil
		}
	}
	forge, err := forgeForRepository(ctx, root, cfg, member)
	if err != nil {
		return nil, err
	}
	current, state, identityErr, err := readStackedPullRequestTarget(ctx, forge, upper, change.PRURL)
	if err != nil {
		return nil, err
	}
	if identityErr != nil {
		return identityErr, nil
	}
	if current != change.FromBranch && current != change.ToBranch {
		return blockedStackTargetChange{reason: fmt.Sprintf("stacked pull request targets %q, expected %q or %q", current, change.FromBranch, change.ToBranch)}, nil
	}
	if current == change.ToBranch {
		return nil, db.CompleteTaskTargetChange(ctx, change.ID)
	}
	if state == "MERGED" || state == "merged" {
		return skippedStackTargetChange{reason: "upper PR/MR was already merged into the lower Task branch"}, nil
	}
	if state != "OPEN" && state != "opened" {
		return blockedStackTargetChange{reason: fmt.Sprintf("stacked pull request is %s and cannot be retargeted", state)}, nil
	}
	if err := updateStackedPullRequestTarget(ctx, forge, change.PRURL, change.ToBranch); err != nil {
		return nil, err
	}
	current, state, identityErr, err = readStackedPullRequestTarget(ctx, forge, upper, change.PRURL)
	if err != nil {
		return nil, err
	}
	if identityErr != nil {
		return identityErr, nil
	}
	if current != change.ToBranch || (state != "OPEN" && state != "opened") {
		return blockedStackTargetChange{reason: fmt.Sprintf("stacked pull request retarget verification found target %q and state %q", current, state)}, nil
	}
	return nil, db.CompleteTaskTargetChange(ctx, change.ID)
}

func readStackedPullRequestTarget(ctx context.Context, forge repositoryForge, task store.Task, prURL string) (target, state string, blocked error, err error) {
	if forge.Kind == "gitlab" {
		number, parseErr := stackedMRNumber(forge, prURL)
		if parseErr != nil {
			return "", "", blockedStackTargetChange{reason: parseErr.Error()}, nil
		}
		mr, viewErr := gitlabMR(ctx, forge, number)
		if viewErr != nil {
			return "", "", nil, viewErr
		}
		if mr.WebURL != prURL || mr.SourceBranch != task.Branch || mr.SourceProjectID == 0 || mr.SourceProjectID != mr.TargetProjectID {
			return "", "", blockedStackTargetChange{reason: "GitLab stacked MR identity does not match the Task"}, nil
		}
		return mr.TargetBranch, mr.State, nil, nil
	}
	output, viewErr := commandOutputArgs(ctx, forge.Root, "gh", "pr", "view", prURL, "--json", "url,state,headRefName,baseRefName")
	if viewErr != nil {
		return "", "", nil, fmt.Errorf("gh pr view: %w: %s", viewErr, strings.TrimSpace(output))
	}
	var pull ghPullRequest
	if err := json.Unmarshal([]byte(output), &pull); err != nil {
		return "", "", nil, fmt.Errorf("decode GitHub pull request: %w", err)
	}
	if pull.URL != prURL || pull.HeadRefName != task.Branch {
		return "", "", blockedStackTargetChange{reason: "GitHub stacked pull request identity does not match the Task"}, nil
	}
	return pull.BaseRefName, pull.State, nil, nil
}

func updateStackedPullRequestTarget(ctx context.Context, forge repositoryForge, prURL, branch string) error {
	if forge.Kind == "gitlab" {
		number, err := stackedMRNumber(forge, prURL)
		if err != nil {
			return err
		}
		output, err := commandOutputArgs(ctx, forge.Root, "glab", "mr", "update", fmt.Sprint(number), "--repo", "https://"+forge.Host+"/"+forge.Path, "--target-branch", branch)
		if err != nil {
			return fmt.Errorf("glab mr update: %w: %s", err, strings.TrimSpace(output))
		}
		return nil
	}
	output, err := commandOutputArgs(ctx, forge.Root, "gh", "pr", "edit", prURL, "--base", branch)
	if err != nil {
		return fmt.Errorf("gh pr edit: %w: %s", err, strings.TrimSpace(output))
	}
	return nil
}

func stackedMRNumber(forge repositoryForge, prURL string) (int, error) {
	reference, err := pullRequestReferenceFromURL(prURL)
	if err != nil {
		return 0, err
	}
	if !strings.EqualFold(reference.Host, forge.Host) || !strings.EqualFold(reference.Owner+"/"+reference.Repo, forge.Path) {
		return 0, fmt.Errorf("GitLab MR URL does not match the configured repository")
	}
	return reference.Number, nil
}

func recordStackedTargetChangeFailure(ctx context.Context, db *store.DB, project store.Project, task store.Task, change store.TaskTargetChange, cause error, now time.Time) error {
	if change.Repo != "" {
		return recordWorkspacePRTaskWatchFailure(ctx, db, project, task, change.Repo, change.PRURL, cause, now)
	}
	return recordPRTaskWatchFailure(ctx, db, project, task, cause, now)
}
