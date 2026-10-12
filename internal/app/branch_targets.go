package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func taskTargetBranch(project store.Project, task store.Task) string {
	if task.TargetBranch != "" {
		return task.TargetBranch
	}
	return project.DefaultBranch
}

func branchSelection(scalar string, byRepo map[string]string, repo, fallback string) (string, bool) {
	branch, selected := fallback, false
	if scalar != "" {
		branch, selected = scalar, true
	}
	if memberBranch, found := byRepo[repo]; found {
		branch, selected = memberBranch, true
	}
	return branch, selected
}

func configTargetBranch(cfg config.Config, repo string, defaultBranch string) (string, bool) {
	branch := cfg.Defaults.TargetBranch
	selected := branch != ""
	if member, found := cfg.Repositories[repo]; found && member.TargetBranch != "" {
		branch, selected = member.TargetBranch, true
	}
	if branch == "" {
		branch = defaultBranch
	}
	return branch, selected
}

func validateBranchMapMembers(field string, branches map[string]string, requested []string) error {
	selected := make(map[string]bool, len(requested))
	for _, name := range requested {
		selected[name] = true
	}
	for name := range branches {
		if !selected[name] {
			return axi.Failure("brief_invalid", fmt.Sprintf("%s names member %s that this Task does not include", field, name), false, "Add the member to `repos:` or remove it from the branch map")
		}
	}
	return nil
}

func selectedBranchRef(ctx context.Context, root, branch, field string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", axi.Failure("brief_invalid", field+" branch cannot be empty", false)
	}
	if _, err := gitOutput(ctx, root, "check-ref-format", "refs/heads/"+branch); err != nil {
		return "", axi.Failure("brief_invalid", fmt.Sprintf("%s branch %q is not a valid Git branch name", field, branch), false, err.Error())
	}
	if _, err := gitOutput(ctx, root, "remote", "get-url", "origin"); err == nil {
		ref := "refs/heads/" + branch
		output, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", ref)
		if err != nil {
			return "", axi.Failure("brief_invalid", "could not validate "+field+" branch "+branch+" on origin", true, err.Error())
		}
		found := false
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == ref {
				found = true
				break
			}
		}
		if !found {
			return "", axi.Failure("brief_invalid", fmt.Sprintf("%s branch %q does not exist on origin", field, branch), false, "Push the branch to origin or choose an existing branch")
		}
		remoteRef := "refs/remotes/origin/" + branch
		if _, err := gitOutput(ctx, root, "rev-parse", "--verify", remoteRef+"^{commit}"); err != nil {
			if _, fetchErr := gitFetch(ctx, root, "origin", ref+":"+remoteRef); fetchErr != nil {
				return "", axi.Failure("brief_invalid", "could not fetch "+field+" branch "+branch+" from origin", true, fetchErr.Error())
			}
		}
		return remoteRef, nil
	}
	localRef := "refs/heads/" + branch
	if _, err := gitOutput(ctx, root, "rev-parse", "--verify", localRef+"^{commit}"); err != nil {
		return "", axi.Failure("brief_invalid", fmt.Sprintf("%s branch %q does not exist", field, branch), false, "Create the branch locally or choose an existing branch")
	}
	return localRef, nil
}

func targetBranchForTask(cfg config.Config, brief dispatch.Brief, repo string, defaultBranch string) (string, bool) {
	branch, configured := configTargetBranch(cfg, repo, defaultBranch)
	if selected, found := brief.TargetByRepo[repo]; found {
		return selected, true
	}
	if brief.Target != "" {
		return brief.Target, true
	}
	return branch, configured
}

func taskTargetRef(ctx context.Context, project store.Project, task store.Task) (string, error) {
	branch := taskTargetBranch(project, task)
	localRef := "refs/heads/" + branch
	if branch == project.DefaultBranch {
		return localRef, nil
	}
	if _, err := gitOutput(ctx, project.Root, "remote", "get-url", "origin"); err != nil {
		if _, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", localRef+"^{commit}"); err != nil {
			return "", err
		}
		return localRef, nil
	}
	remoteRef := "refs/remotes/origin/" + branch
	if _, err := gitFetch(ctx, project.Root, "origin", branch+":"+remoteRef); err != nil {
		return "", err
	}
	return remoteRef, nil
}

func landedTaskTargetRef(ctx context.Context, project store.Project, task store.Task) (string, error) {
	if task.LandingMode != "local" {
		return taskTargetRef(ctx, project, task)
	}
	ref := "refs/heads/" + taskTargetBranch(project, task)
	if _, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
		return "", err
	}
	return ref, nil
}

func ensureLocalTaskTarget(ctx context.Context, project store.Project, task store.Task) error {
	branch := taskTargetBranch(project, task)
	if branch == project.DefaultBranch {
		return nil
	}
	if _, err := gitOutput(ctx, project.Root, "remote", "get-url", "origin"); err != nil {
		if _, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}"); err != nil {
			return err
		}
		return nil
	}
	_, err := gitFetch(ctx, project.Root, "origin", branch+":refs/heads/"+branch)
	return err
}
