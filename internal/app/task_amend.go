package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
)

func (s *Service) taskAmend(out *axi.Context, args []string) error {
	parsed, err := parseArgs("task amend", args, map[string]flagSpec{"head": {}, "scope": {}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 2 || parsed.Flags["head"] == "" || strings.TrimSpace(parsed.Flags["scope"]) == "" {
		return axi.Usage("task amend requires <task> <repo> --head <sha> --scope <addition> --user-approved <quote>")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	ctx := out.Context
	project, err := s.projectForCWD(ctx, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx, db, project); err != nil {
		return err
	}
	if quote := parsed.Flags["user-approved"]; strings.TrimSpace(quote) == "" {
		return axi.Failure("user_approval_required", "a scope amendment requires the User's exact approval quote", false)
	}
	if project.IsWorkspace() || project.IsDirectory() || parsed.Positionals[1] != project.Name {
		return axi.Failure("scope_repository_mismatch", "select the existing repository of a single-repository Task", false)
	}
	task, err := s.currentTask(ctx, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.WorktreePath == "" || task.Branch == "" {
		return axi.Failure("scope_amendment_refused", "Task has no Sandbox branch", false)
	}
	authorLock, err := acquireTaskBranchAuthorLock(ctx, task.WorktreePath, task.Branch)
	if err != nil {
		return err
	}
	defer func() { _ = authorLock.Close() }()
	roundDir := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "greenhouse")
	reviewLock, busy, err := acquireParallelReviewLock(parallelReviewLockPath(roundDir, project.Name))
	if err != nil {
		return err
	}
	if busy {
		return axi.Failure("scope_review_in_flight", "wait for the current Greenhouse review to settle before amending scope", true)
	}
	defer func() { _ = releaseParallelReviewLock(reviewLock) }()
	if pending, err := parallelReviewPending(filepath.Join(roundDir, project.Name+".pending.json")); err != nil {
		return err
	} else if pending {
		return axi.Failure("scope_review_in_flight", "resume or recover the pending Greenhouse review before amending scope", false)
	}
	task, err = db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return err
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	if task.State != store.StateWorking || task.Type != "change" || task.MergeMode != "pr" || task.PRURL == "" || plan == nil || !greenhouseFullPipelineTask(task) {
		return axi.Failure("scope_amendment_refused", "scope amendments require a working Greenhouse full-pipeline Change Task with an existing PR or MR", false)
	}
	if err := requireCurrentTaskBranchAuthor(ctx, db, task); err != nil {
		return err
	}
	oldHead, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+task.Branch)
	if err != nil {
		return err
	}
	newHead, err := taskBranchAuthorHead(ctx, task.WorktreePath, task.Branch)
	if err != nil {
		return err
	}
	if newHead != parsed.Flags["head"] {
		return axi.Failure("scope_head_mismatch", "--head must match the full current Task branch SHA", false)
	}
	verified, err := db.WasVerifiedPRHead(ctx, task.ID, task.PRURL, oldHead)
	if err != nil {
		return err
	}
	if oldHead == "" || !verified || newHead == oldHead || !gitAncestor(ctx, task.WorktreePath, oldHead, newHead) {
		return axi.Failure("scope_amendment_refused", "the Task branch must advance from its verified PR or MR head without rewriting history", false)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	if err := refuseMergedTaskPullRequest(ctx, db, project, cfg, task, ""); err != nil {
		return err
	}
	forge, err := forgeForRepository(ctx, project.Root, cfg, "")
	if err != nil {
		return err
	}
	if err := validateWorkerPullRequestWithRemote(ctx, project, task, forge, task.PRURL, oldHead, false); err != nil {
		return err
	}
	amendment := taskplan.ScopeAmendment{Repository: project.Name, Scope: parsed.Flags["scope"], Quote: parsed.Flags["user-approved"], Actor: "lead", Source: taskplan.ScopeAmendmentSource, PRURL: task.PRURL, OldHead: oldHead, NewHead: newHead}
	if n := len(plan.ScopeAmendments); n > 0 {
		last := plan.ScopeAmendments[n-1]
		amendment.Revision = last.Revision
		if amendment == last {
			return printScopeAmendment(out, task, last)
		}
	}
	revised, err := plan.AmendScope(amendment)
	if err != nil {
		return axi.Usage(err.Error())
	}
	if err := db.ReviseTaskPlan(ctx, task, revised); err != nil {
		return err
	}
	amendment.Revision = revised.Revision
	return printScopeAmendment(out, task, amendment)
}

func printScopeAmendment(out *axi.Context, task store.Task, amendment taskplan.ScopeAmendment) error {
	return out.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)}, {Key: "scope_amendment", Value: amendment},
		{Key: "help", Value: []string{fmt.Sprintf("Ask the Worker to run `posse signal done \"<summary>\" --head %s` with the current turn token; do not publish until Greenhouse passes", amendment.NewHead)}},
	})
}
