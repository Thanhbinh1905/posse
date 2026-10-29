package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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

// defaultBranchBase returns the ref a Task starting from the default branch
// branches from: the local branch, else origin's copy. It refuses before
// admission when neither exists, so the failure breaks no Sandbox.
func defaultBranchBase(ctx context.Context, project store.Project) (string, error) {
	if defaultBranchUnborn(ctx, projectRepoTarget(project)) {
		return project.DefaultBranch, nil
	}
	for _, ref := range []string{"refs/heads/" + project.DefaultBranch, "refs/remotes/origin/" + project.DefaultBranch} {
		_, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err == nil {
			return strings.TrimPrefix(ref, "refs/heads/"), nil
		}
		if !gitRefMissing(err) {
			return "", err
		}
	}
	return "", axi.Failure("default_branch_missing", fmt.Sprintf("Project %s has no branch %s to start Tasks from", project.Name, project.DefaultBranch), false,
		"Create it in "+project.Root+" with `git branch "+project.DefaultBranch+" <commit>`, then retry")
}

// commitsAheadOfBase counts the commits on HEAD that base lacks. While base is
// the repository's default branch with no commit yet, every commit on HEAD is
// ahead of it; any other missing base is an error.
func commitsAheadOfBase(ctx context.Context, worktree, base string) (string, error) {
	_, err := gitOutput(ctx, worktree, "rev-parse", "--verify", "--quiet", base+"^{commit}")
	if err == nil {
		return gitOutput(ctx, worktree, "rev-list", "--count", base+"..HEAD")
	}
	if !gitRefMissing(err) {
		return "", err
	}
	common, err := gitOutput(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !defaultBranchUnborn(ctx, repoTarget{Root: common, DefaultBranch: base}) {
		return "", fmt.Errorf("base ref %s does not exist", base)
	}
	if _, err := gitOutput(ctx, worktree, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if gitRefMissing(err) {
			return "0", nil
		}
		return "", err
	}
	return gitOutput(ctx, worktree, "rev-list", "--count", "HEAD")
}

// revisionOrEmptyTree returns rev, or the empty tree while rev names a branch
// with no commit yet, so a diff before the first commit lists every file.
func revisionOrEmptyTree(ctx context.Context, dir, rev string) (string, error) {
	_, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err == nil || !gitRefMissing(err) {
		return rev, err
	}
	return gitOutput(ctx, dir, "hash-object", "-t", "tree", "/dev/null")
}

// gitRefMissing reports whether `git rev-parse --verify --quiet` failed only
// because the revision does not exist.
func gitRefMissing(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}
