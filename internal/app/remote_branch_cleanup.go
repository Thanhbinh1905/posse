package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// deleteMergedRemoteBranch removes only the recorded Task head, after a fresh
// forge read and an atomic Git lease check against the recorded merged head.
func deleteMergedRemoteBranch(ctx context.Context, project store.Project, task store.Task, cfg config.Config, headSHA string) (bool, error) {
	if task.Branch == "" || task.PRURL == "" || headSHA == "" || task.Branch == project.DefaultBranch || task.Branch == taskTargetBranch(project, task) {
		return false, nil
	}
	forge, err := forgeForRepository(ctx, project.Root, cfg, "")
	if err != nil {
		return false, nil
	}
	state, head, branch, err := readRemotePRHead(ctx, forge, task.PRURL)
	if err != nil || state != "MERGED" || head != headSHA || branch != task.Branch {
		return false, nil
	}
	ref := "refs/heads/" + task.Branch
	remoteHead, err := gitOutput(ctx, project.Root, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return false, nil
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
		return false, nil
	}
	remote, err := gitOutput(ctx, project.Root, "ls-remote", "--heads", "origin", ref)
	if err != nil {
		return false, nil
	}
	if strings.TrimSpace(remote) != headSHA+"\t"+ref {
		return false, nil
	}
	// Re-read immediately before deletion. The push lease also preserves a
	// concurrent branch advance between this read and the delete request.
	state, head, branch, err = readRemotePRHead(ctx, forge, task.PRURL)
	if err != nil || state != "MERGED" || head != headSHA || branch != task.Branch {
		return false, nil
	}
	remote, err = gitOutput(ctx, project.Root, "ls-remote", "--heads", "origin", ref)
	if err != nil || strings.TrimSpace(remote) != headSHA+"\t"+ref {
		return false, nil
	}
	_, err = gitOutput(ctx, project.Root, "push", "--force-with-lease="+ref+":"+headSHA, "origin", ":"+ref)
	if err != nil {
		return false, nil
	}
	return true, nil
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
	output, err := commandOutputArgs(ctx, forge.Root, "gh", "pr", "view", prURL, "--json", "url,state,headRefOid,headRefName,headRepository")
	if err != nil {
		return "", "", "", err
	}
	var result struct {
		URL            string `json:"url"`
		State          string `json:"state"`
		Head           string `json:"headRefOid"`
		Branch         string `json:"headRefName"`
		HeadRepository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"headRepository"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", "", "", err
	}
	if result.URL != prURL || !strings.EqualFold(result.HeadRepository.NameWithOwner, forge.Path) {
		return "", "", "", fmt.Errorf("forge returned a different pull request or source repository")
	}
	return strings.ToUpper(result.State), result.Head, result.Branch, nil
}
