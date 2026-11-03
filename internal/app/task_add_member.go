package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func (s *Service) taskAddMember(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("task add-member", args, map[string]flagSpec{"base": {}, "target": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 2 {
		return axi.Usage("task add-member requires <task> <repo> [--base <ref>] [--target <ref>]")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if !project.IsWorkspace() {
		return axi.Failure("task_not_workspace", "Task belongs to a repository Project, not a workspace", false)
	}
	if task.WorktreePath == "" {
		return axi.Failure("sandbox_missing", "running workspace Task has no Sandbox path", false)
	}
	if task.Type != "change" {
		return axi.Failure("task_not_change", "only a workspace Change Task can add a Member", false)
	}
	if task.State != store.StateWorking {
		return axi.Failure("task_not_running", "Task must be running to add a Member", false)
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return err
	}
	if plan == nil || plan.Execution.Scope != "workspace" {
		return axi.Failure("admitted_plan_missing", "workspace Change Task has no admitted workspace plan", false)
	}
	repoName := parsed.Positionals[1]
	for _, member := range plan.Execution.Members {
		if member.Name == repoName {
			return axi.Failure("member_already_added", "repository "+repoName+" is already in this Task", false)
		}
	}
	cfg, err := s.prepareProjectMode(ctx.Context, db, project, true, false)
	if err != nil {
		return err
	}
	repos, err := db.ProjectRepos(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	var repo *store.ProjectRepo
	for i := range repos {
		if repos[i].Name == repoName && repos[i].Status == store.RepoActive {
			repo = &repos[i]
			break
		}
	}
	if repo == nil {
		return axi.Failure("not_project_member", "repository "+repoName+" is not an active Member of this Project", false, "Run `posse project show` to inspect Project Members")
	}
	if !safeWorkspaceRootPath(repo.Path) {
		return axi.Failure("member_path_invalid", "Project Member has an unsafe Sandbox path", false)
	}
	root := filepath.Join(project.Root, repo.Path)
	remote := originHost(ctx.Context, root)
	member := taskMember{repoTarget: repoTarget{Name: repo.Name, Root: root, DefaultBranch: repo.DefaultBranch}, Path: repo.Path}
	member.MergeMode = memberMergeMode(cfg, repo.Name, remote)
	brief, err := admittedChangeBrief(plan)
	if err != nil {
		return err
	}
	if selected := parsed.Flags["target"]; selected != "" {
		brief.Target, brief.TargetByRepo = selected, nil
	}
	var targetSelected bool
	member.TargetBranch, targetSelected = targetBranchForTask(cfg, brief, repo.Name, repo.DefaultBranch)
	if targetSelected {
		member.BaseRef, err = selectedBranchRef(ctx.Context, root, member.TargetBranch, "target")
	} else {
		member.BaseRef = memberBaseRef(member.repoTarget, remote != "")
	}
	if err != nil {
		return err
	}
	if selected := parsed.Flags["base"]; selected != "" {
		brief.Base, brief.BaseByRepo = selected, nil
		member.BaseRef, err = selectedBranchRef(ctx.Context, root, selected, "base")
	} else if base, explicit := branchSelection(brief.Base, brief.BaseByRepo, repo.Name, ""); explicit {
		member.BaseRef, err = selectedBranchRef(ctx.Context, root, base, "base")
	} else if baseFromTarget(cfg, brief, repo.Name, repo.DefaultBranch) {
		member.BaseRef, err = selectedBranchRef(ctx.Context, root, member.TargetBranch, "target")
	}
	if err != nil {
		return err
	}
	if member.MergeMode == "pr" && remote == "" {
		return axi.Failure("member_invalid", "member "+repoName+" has no origin and cannot use PR delivery", false)
	}
	deleteBranch := cfg.MemberDeleteBranchOnMerge(repoName)
	selected := taskplan.Member{Name: repoName, Path: repo.Path, Base: member.BaseRef, Target: member.TargetBranch, Delivery: member.MergeMode,
		Review: cfg.MemberReview(repoName), ReviewSource: "defaults.review", DeleteBranchOnMerge: &deleteBranch}
	if cfg.Repositories[repoName].Review != "" {
		selected.ReviewSource = "repositories." + repoName + ".review"
	}
	if pinsBase(cfg, brief, repo.Name, repo.DefaultBranch) {
		if selected.BaseCommit, err = selectedBaseCommit(ctx.Context, root, member.BaseRef); err != nil {
			return err
		}
	}
	sandboxMemberPath := filepath.Join(task.WorktreePath, member.Path)
	var base string
	if err := withSandboxStateLock(ctx.Context, db, func() error {
		latest, err := db.TaskByID(ctx.Context, project.ID, task.ID)
		if err != nil {
			return err
		}
		if latest.State != store.StateWorking || latest.PlanJSON != task.PlanJSON {
			return axi.Failure("task_changed", "Task state or admitted plan changed while adding the Member", true)
		}
		if err := validateWorkspaceSandboxMemberPath(task.WorktreePath, member.Path); err != nil {
			return axi.Failure("sandbox_member_path_invalid", "Task Sandbox cannot safely contain the new Member", false, err.Error())
		}
		if err := os.MkdirAll(filepath.Dir(sandboxMemberPath), 0o700); err != nil {
			return err
		}
		base, err = gitOutput(ctx.Context, root, "rev-parse", "--verify", member.BaseRef+"^{commit}")
		if err != nil && strings.HasPrefix(member.BaseRef, "refs/remotes/origin/") {
			branch := strings.TrimPrefix(member.BaseRef, "refs/remotes/origin/")
			if _, fetchErr := gitFetch(ctx.Context, root, "origin", "refs/heads/"+branch); fetchErr != nil {
				return fetchErr
			}
			base, err = gitOutput(ctx.Context, root, "rev-parse", "--verify", member.BaseRef+"^{commit}")
		}
		if err != nil {
			return err
		}
		if _, err := gitOutput(ctx.Context, root, "worktree", "add", "-b", task.Branch, sandboxMemberPath, base); err != nil {
			return axi.Failure("member_checkout_failed", "could not add "+repoName+" to the Task Sandbox", false, err.Error())
		}
		revised := *plan
		revised.Execution.Members = append(append([]taskplan.Member(nil), plan.Execution.Members...), selected)
		revised.Revision++
		revised.PreviousSHA256 = plan.SHA256()
		if err := db.ReviseTaskPlanWithRepo(ctx.Context, task, revised, store.TaskRepo{Repo: repoName, WorktreePath: sandboxMemberPath, BaseRef: base, TargetBranch: member.TargetBranch, MergeMode: member.MergeMode}); err != nil {
			if current, readErr := db.TaskByID(ctx.Context, project.ID, task.ID); readErr == nil && current.PlanJSON == task.PlanJSON {
				_, _ = gitOutput(ctx.Context, root, "worktree", "remove", "--force", sandboxMemberPath)
				_, _ = gitOutput(ctx.Context, root, "branch", "-D", task.Branch)
			}
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	instruction := fmt.Sprintf("Added workspace Member %s to Task %s. Its Sandbox path is %s. Include this Member in the change if needed.", repoName, taskIDString(task.Seq), sandboxMemberPath)
	return s.send(ctx, []string{taskIDString(task.Seq), instruction})
}

func admittedChangeBrief(plan *taskplan.Plan) (dispatch.Brief, error) {
	for _, input := range plan.Inputs {
		if input.Role != "brief" {
			continue
		}
		brief, err := dispatch.ParseBriefText(string(input.Bytes))
		if err != nil {
			return dispatch.Brief{}, err
		}
		return brief, nil
	}
	return dispatch.Brief{}, axi.Failure("admitted_plan_invalid", "workspace Change Task has no admitted Brief", false)
}
