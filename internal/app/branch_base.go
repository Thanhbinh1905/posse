package app

import (
	"context"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// selectedBaseCommit reads the selected branch's tip without fetching or moving
// any ref. The branch name remains in the plan alongside this pinned commit.
func selectedBaseCommit(ctx context.Context, root, ref string) (string, error) {
	if branch, remote := strings.CutPrefix(ref, "refs/remotes/origin/"); remote {
		output, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(output, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == "refs/heads/"+branch {
				return fields[0], nil
			}
		}
		return "", axi.Failure("brief_invalid", "selected base branch disappeared from origin", true)
	}
	return gitOutput(ctx, root, "rev-parse", "--verify", ref+"^{commit}")
}

func taskCheckoutBase(ctx context.Context, root string, task store.Task, member, fallback string) (string, error) {
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return fallback, err
	}
	commit, ref := plan.Execution.BaseCommit, plan.Execution.Base
	if member != "" {
		commit = ""
		for _, selected := range plan.Execution.Members {
			if selected.Name == member {
				commit, ref = selected.BaseCommit, selected.Base
				break
			}
		}
	}
	if commit == "" {
		return fallback, nil
	}
	if _, err := gitOutput(ctx, root, "cat-file", "-e", commit+"^{commit}"); err != nil {
		if branch, remote := strings.CutPrefix(ref, "refs/remotes/origin/"); remote {
			if _, err := gitFetch(ctx, root, "origin", "refs/heads/"+branch); err != nil {
				return "", err
			}
		}
		if _, err := gitOutput(ctx, root, "cat-file", "-e", commit+"^{commit}"); err != nil {
			return "", axi.Failure("plan_base_unavailable", "the admitted base commit is no longer available", false, "Restore the selected commit; Posse will not substitute today's branch tip")
		}
	}
	return commit, nil
}
