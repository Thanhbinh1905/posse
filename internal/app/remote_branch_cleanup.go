package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// deleteMergedRemoteBranch removes only the recorded Task head, after a fresh
// forge read and an atomic Git lease check against the recorded merged head.
// It returns an error with the cause when a read or the push fails, and false
// with no error when the branch must stay or is already gone.
func deleteMergedRemoteBranch(ctx context.Context, project store.Project, task store.Task, cfg config.Config, headSHA string) (bool, error) {
	if task.Branch == "" || task.PRURL == "" || headSHA == "" || task.Branch == project.DefaultBranch || task.Branch == taskTargetBranch(project, task) {
		return false, nil
	}
	forge, err := forgeForRepository(ctx, project.Root, cfg, "")
	if err != nil {
		return false, fmt.Errorf("resolve forge: %w", err)
	}
	merged, err := remotePRMergedAt(ctx, forge, task, headSHA)
	if err != nil || !merged {
		return false, err
	}
	ref := "refs/heads/" + task.Branch
	remoteHead, err := gitOutput(ctx, project.Root, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return false, err
	}
	defaultKnown := false
	for _, line := range strings.Split(remoteHead, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") {
			defaultKnown = true
			if strings.TrimPrefix(strings.SplitN(line, "\t", 2)[0], "ref: ") == ref {
				return false, nil
			}
		}
	}
	if !defaultKnown {
		return false, fmt.Errorf("origin HEAD does not name a default branch: %q", remoteHead)
	}
	if atHead, err := remoteBranchAt(ctx, project.Root, ref, headSHA); err != nil || !atHead {
		return false, err
	}
	// Re-read immediately before deletion. The push lease also preserves a
	// concurrent branch advance between this read and the delete request.
	if merged, err := remotePRMergedAt(ctx, forge, task, headSHA); err != nil || !merged {
		return false, err
	}
	if atHead, err := remoteBranchAt(ctx, project.Root, ref, headSHA); err != nil || !atHead {
		return false, err
	}
	if _, err := gitOutput(ctx, project.Root, "push", "--force-with-lease="+ref+":"+headSHA, "origin", ":"+ref); err != nil {
		return false, err
	}
	return true, nil
}

func remotePRMergedAt(ctx context.Context, forge repositoryForge, task store.Task, headSHA string) (bool, error) {
	state, head, branch, err := readRemotePRHead(ctx, forge, task.PRURL)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", task.PRURL, err)
	}
	return state == "MERGED" && head == headSHA && branch == task.Branch, nil
}

func remoteBranchAt(ctx context.Context, root, ref, headSHA string) (bool, error) {
	remote, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", ref)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(remote) == headSHA+"\t"+ref, nil
}

// cleanUpMergedRemoteBranch deletes a merged Task branch from origin. A failed
// deletion is recorded with its cause so a later watch pass or `posse pool
// prune` retries it; a settled one clears that record.
func cleanUpMergedRemoteBranch(ctx context.Context, db *store.DB, project store.Project, task store.Task, repo string, cfg config.Config, headSHA string) (bool, string, error) {
	removed, cause := deleteMergedRemoteBranch(ctx, project, task, cfg, headSHA)
	if cause != nil {
		failure := store.RemoteBranchCleanup{ProjectID: project.ID, TaskID: task.ID, Repo: repo, Branch: task.Branch, HeadSHA: headSHA, Cause: cause.Error()}
		return false, failure.Cause, db.RecordRemoteBranchCleanupFailure(ctx, failure)
	}
	return removed, "", db.ClearRemoteBranchCleanup(ctx, task.ID, repo)
}

func retryRemoteBranchCleanups(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	cleanups, err := db.RemoteBranchCleanups(ctx, project.ID)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, cleanup := range cleanups {
		if now.Before(time.UnixMilli(cleanup.FailedAt).Add(remoteBranchCleanupBackoff(cleanup.Attempts))) {
			continue
		}
		if _, _, err := retryRemoteBranchCleanup(ctx, db, project, cfg, cleanup); err != nil {
			return err
		}
	}
	return nil
}

func retryRemoteBranchCleanup(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, cleanup store.RemoteBranchCleanup) (bool, string, error) {
	task, err := db.TaskByID(ctx, project.ID, cleanup.TaskID)
	if err != nil {
		return false, "", err
	}
	if cleanup.Repo != "" {
		members, err := workspaceMembers(ctx, db, project, task)
		if err != nil {
			return false, "", err
		}
		found := false
		for _, member := range members {
			if member.repo.Repo == cleanup.Repo {
				project, task, found = member.project, member.task, true
			}
		}
		if !found {
			return false, "", db.ClearRemoteBranchCleanup(ctx, cleanup.TaskID, cleanup.Repo)
		}
	}
	return cleanUpMergedRemoteBranch(ctx, db, project, task, cleanup.Repo, cfg, cleanup.HeadSHA)
}

func remoteBranchCleanupBackoff(attempts int) time.Duration {
	if attempts > 7 {
		return time.Hour
	}
	return time.Minute << (attempts - 1)
}

func readRemotePRHead(ctx context.Context, forge repositoryForge, prURL string) (state, head, branch string, err error) {
	number, err := forgeReference(prURL, forge)
	if err != nil {
		return "", "", "", err
	}
	if forge.Kind == "gitlab" {
		mr, readErr := gitlabMR(ctx, forge, number)
		if readErr != nil {
			return "", "", "", readErr
		}
		state = strings.ToUpper(mr.State)
		if state == "OPENED" {
			state = "OPEN"
		}
		if mr.WebURL != prURL || mr.SourceProjectID == 0 || mr.SourceProjectID != mr.TargetProjectID {
			return "", "", "", fmt.Errorf("forge pull request source does not match its target repository")
		}
		return state, mr.SHA, mr.SourceBranch, nil
	}
	output, err := commandStdoutArgs(ctx, forge.Root, "gh", "pr", "view", prURL, "--json", "url,state,headRefOid,headRefName,headRepository,headRepositoryOwner")
	if err != nil {
		return "", "", "", err
	}
	var result struct {
		URL            string `json:"url"`
		State          string `json:"state"`
		Head           string `json:"headRefOid"`
		Branch         string `json:"headRefName"`
		HeadRepository struct {
			Name          string `json:"name"`
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"headRepository"`
		HeadRepositoryOwner struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", "", "", err
	}
	// Older gh releases, such as 2.46, omit headRepository.nameWithOwner.
	repository := result.HeadRepository.NameWithOwner
	if repository == "" && result.HeadRepositoryOwner.Login != "" && result.HeadRepository.Name != "" {
		repository = result.HeadRepositoryOwner.Login + "/" + result.HeadRepository.Name
	}
	if result.URL != prURL || !strings.EqualFold(repository, forge.Path) {
		return "", "", "", fmt.Errorf("forge returned a different pull request or source repository")
	}
	return strings.ToUpper(result.State), result.Head, result.Branch, nil
}

// mergedRemoteBranchItems lists the remote posse/* branches of merged or
// torn-down Tasks whose pull request a fresh forge read shows MERGED at the
// exact branch head.
func mergedRemoteBranchItems(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, tasks []store.Task) ([]pruneItem, error) {
	pending, err := db.RemoteBranchCleanups(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	failures := map[string]string{}
	for _, cleanup := range pending {
		failures[fmt.Sprintf("%d/%s", cleanup.TaskID, cleanup.Repo)] = cleanup.Cause
	}
	remoteHeads := map[string]map[string]string{}
	listErrors := map[string]error{}
	items := []pruneItem{}
	for _, task := range tasks {
		if task.State != store.StateMerged && task.State != store.StateTornDown {
			continue
		}
		type candidate struct {
			project store.Project
			task    store.Task
			repo    string
		}
		var candidates []candidate
		if project.IsWorkspace() {
			members, err := workspaceMembers(ctx, db, project, task)
			if err != nil {
				return nil, err
			}
			for _, member := range members {
				if member.repo.State == store.TaskRepoMerged && member.repo.MergeMode == "pr" {
					candidates = append(candidates, candidate{member.project, member.task, member.repo.Repo})
				}
			}
		} else if deliveredByPullRequest(task.MergeMode) {
			candidates = append(candidates, candidate{project, task, ""})
		}
		for _, candidate := range candidates {
			if !strings.HasPrefix(candidate.task.Branch, "posse/") || candidate.task.PRURL == "" {
				continue
			}
			item := pruneItem{kind: "remote-branch", label: candidate.task.Branch, path: candidate.project.Root, state: string(task.State), seq: task.Seq, taskID: task.ID, repo: candidate.repo}
			root := candidate.project.Root
			if _, listed := remoteHeads[root]; !listed && listErrors[root] == nil {
				listing, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", "refs/heads/posse/*")
				if err != nil {
					listErrors[root] = err
					items = append(items, pruneItem{kind: "remote-branch", label: "posse/*", path: root, state: "unverified", reason: err.Error()})
				} else {
					remoteHeads[root] = map[string]string{}
					for _, line := range strings.Split(listing, "\n") {
						if sha, ref, found := strings.Cut(line, "\t"); found {
							remoteHeads[root][strings.TrimPrefix(ref, "refs/heads/")] = sha
						}
					}
				}
			}
			if listErrors[root] != nil {
				continue
			}
			item.sha = remoteHeads[root][candidate.task.Branch]
			if item.sha == "" {
				continue
			}
			forge, err := forgeForRepository(ctx, candidate.project.Root, cfg, "")
			merged := false
			if err == nil {
				merged, err = remotePRMergedAt(ctx, forge, candidate.task, item.sha)
			}
			if err != nil {
				item.state, item.reason = "unverified", err.Error()
				items = append(items, item)
				continue
			}
			if !merged {
				continue
			}
			item.reason = "PR " + candidate.task.PRURL + " merged at the branch head"
			if cause := failures[fmt.Sprintf("%d/%s", task.ID, candidate.repo)]; cause != "" {
				item.reason += "; the last deletion failed: " + cause
			}
			items = append(items, item)
		}
	}
	return items, nil
}

func applyRemoteBranchPruneItem(ctx context.Context, db *store.DB, home string, project store.Project, item pruneItem) error {
	if item.state == "unverified" {
		return skippedPruneBranch(item, item.reason)
	}
	task, err := db.TaskByID(ctx, project.ID, item.taskID)
	if err != nil {
		return err
	}
	if task.State != store.StateMerged && task.State != store.StateTornDown {
		return skippedPruneBranch(item, "its Task changed since prune was planned")
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	removed, cause, err := retryRemoteBranchCleanup(ctx, db, project, cfg, store.RemoteBranchCleanup{TaskID: item.taskID, Repo: item.repo, HeadSHA: item.sha})
	if err != nil {
		return err
	}
	if cause != "" {
		return skippedPruneBranch(item, cause)
	}
	if !removed {
		return skippedPruneBranch(item, "the remote branch is no longer at the merged PR head")
	}
	return nil
}
