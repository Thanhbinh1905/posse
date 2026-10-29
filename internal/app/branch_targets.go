package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
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
		// Admission is read-only. Sandbox acquisition fetches a selected base
		// only after all choices have been admitted.
		return "refs/remotes/origin/" + branch, nil
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

// baseFromTarget reports whether a Change Task without an explicit base starts
// from its selected non-default target branch rather than the default branch.
func baseFromTarget(cfg config.Config, brief dispatch.Brief, repo string, defaultBranch string) bool {
	if brief.Type != "change" {
		return false
	}
	if _, explicit := branchSelection(brief.Base, brief.BaseByRepo, repo, ""); explicit {
		return false
	}
	target, selected := targetBranchForTask(cfg, brief, repo, defaultBranch)
	return selected && target != defaultBranch
}

// pinsBase reports whether admission freezes the start commit. A base taken
// from the target stays live after a landed prerequisite, which lands later.
func pinsBase(cfg config.Config, brief dispatch.Brief, repo string, defaultBranch string) bool {
	if _, explicit := branchSelection(brief.Base, brief.BaseByRepo, repo, ""); explicit {
		return true
	}
	return baseFromTarget(cfg, brief, repo, defaultBranch) && !hasLandedPrerequisite(brief.After)
}

// startPlanFields adds a Change Task's branch plan and all admission warnings
// to the `posse start` result.
func startPlanFields(cfg config.Config, project store.Project, brief dispatch.Brief, plan taskplan.Plan) axi.Object {
	var fields axi.Object
	warnings := briefSkillWarnings(cfg, brief.Type, brief.Title+"\n"+brief.Body)
	if brief.Type == "change" {
		branches, branchWarnings := startBranchPlan(project, plan)
		fields = append(fields, axi.Field{Key: "branches", Value: branches})
		warnings = append(warnings, branchWarnings...)
	}
	if len(warnings) > 0 {
		fields = append(fields, axi.Field{Key: "warnings", Value: warnings})
	}
	return fields
}

// startBranchPlan lists each repository's admitted base and target, with a
// warning for every base that is not its target.
func startBranchPlan(project store.Project, plan taskplan.Plan) ([]axi.Object, []any) {
	type branchPlan struct{ repo, base, commit, target string }
	selected := []branchPlan{{project.Name, plan.Execution.Base, plan.Execution.BaseCommit, plan.Execution.Target}}
	if project.IsWorkspace() {
		selected = selected[:0]
		for _, member := range plan.Execution.Members {
			selected = append(selected, branchPlan{member.Name, member.Base, member.BaseCommit, member.Target})
		}
	}
	rows, warnings := []axi.Object{}, []any{}
	for _, branch := range selected {
		base := strings.TrimPrefix(strings.TrimPrefix(branch.base, "refs/remotes/origin/"), "refs/heads/")
		rows = append(rows, axi.Object{{Key: "repo", Value: branch.repo}, {Key: "base", Value: base}, {Key: "base_commit", Value: branch.commit}, {Key: "target", Value: branch.target}})
		if base != branch.target {
			warnings = append(warnings, fmt.Sprintf("%s: base %s differs from target %s", branch.repo, base, branch.target))
		}
	}
	return rows, warnings
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

func mergedTaskTargetRef(ctx context.Context, project store.Project, task store.Task) (string, error) {
	if task.MergeMode != "local" {
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
