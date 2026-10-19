package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A workspace Task Merges member by member, each through its own Merge Mode, and
// the Task is merged only when every member with commits has Merged. The helpers
// of the single-repository path run on a per-member view of the Project and Task.

const workspaceMergeIntent = "merge --workspace"

type memberMerge struct {
	repo    store.TaskRepo
	target  repoTarget
	project store.Project // the Project seen as this member: its root and default branch
	task    store.Task    // the Task seen in this member: its worktree, gated commit and PR
}

func (s *Service) workspaceMembers(ctx context.Context, db *store.DB, project store.Project, task store.Task) ([]memberMerge, error) {
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	members := make([]memberMerge, 0, len(repos))
	for _, repo := range repos {
		target, err := s.projectTarget(ctx, db, project, repo.Repo)
		if err != nil {
			return nil, err
		}
		view := project
		view.Root, view.DefaultBranch, view.Kind = target.Root, target.DefaultBranch, store.ProjectKindRepo
		memberTask := task
		memberTask.WorktreePath, memberTask.BaseRef, memberTask.TargetBranch, memberTask.MergeMode = repo.WorktreePath, repo.BaseRef, repo.TargetBranch, repo.MergeMode
		memberTask.GatedSHA, memberTask.PRURL, memberTask.MergedRef = repo.GatedSHA, repo.PRURL, repo.MergedRef
		memberTask.Title = repo.Repo + ": " + task.Title
		members = append(members, memberMerge{repo: repo, target: target, project: view, task: memberTask})
	}
	return members, nil
}

func memberGates(cfg config.Config, name string) []string {
	if repository, ok := cfg.Repositories[name]; ok && repository.Gate != nil {
		return repository.Gate
	}
	return cfg.Defaults.Gate
}

func (s *Service) mergeWorkspace(out *axi.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, merge bool, quote string) (returnErr error) {
	ctx := out.Context
	if task.State != store.StateDone && task.State != store.StateMerging {
		return axi.Failure("merge_refused", "Task must be done or merging before Merge", false, "Run `posse show "+taskIDString(task.Seq)+"` to inspect its state")
	}
	if merge && task.AutonomyMerge != "auto" && quote == "" {
		return axi.Failure("merge_approval_required", "merging a workspace Task needs User approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
	}
	members, err := s.workspaceMembers(ctx, db, project, task)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return axi.Failure("merge_refused", "workspace Task has no member repositories", false)
	}
	intent, err := s.startTaskIntent(ctx, db, project.ID, task.ID, workspaceMergeIntent)
	if err != nil {
		return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	intentOpen := true
	defer func() {
		if !intentOpen {
			return
		}
		if finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID); returnErr == nil && finishErr != nil && !errors.Is(finishErr, store.ErrIntentOwned) {
			returnErr = finishErr
		}
	}()
	if task.State == store.StateDone {
		if err := s.openWorkspaceMerge(ctx, db, project, cfg, task, intent, members); err != nil {
			_ = s.deliverNotices(ctx, db, project)
			return err
		}
		task.State = store.StateMerging
		if members, err = s.workspaceMembers(ctx, db, project, task); err != nil {
			return err
		}
	}
	if plan, planErr := task.AdmittedPlan(); planErr != nil {
		return admittedPlanFailure(planErr)
	} else if autoStartReviewLoop(plan) {
		home, homeErr := s.homePath()
		if homeErr != nil {
			return homeErr
		}
		if finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID); finishErr != nil {
			return finishErr
		}
		intentOpen = false
		started, startErr := s.maybeAutoStartReviewRound(out, db, home, project, task)
		if startErr != nil {
			return startErr
		}
		if started {
			return nil
		}
		intent, err = s.startTaskIntent(ctx, db, project.ID, task.ID, workspaceMergeIntent)
		if err != nil {
			return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
		}
		intentOpen = true
	}
	if !merge {
		for _, member := range members {
			if member.repo.State != store.TaskRepoMerge || member.repo.MergeMode != "pr" || member.repo.GatedSHA == "" {
				continue
			}
			if err := s.requirePRTargetFresh(ctx, db, member.project, member.task, member.repo.Repo, member.repo.MergeMode, member.repo.GatedSHA); err != nil {
				return prefixFailure(member.repo.Repo, err)
			}
		}
	}
	if merge {
		if quote != "" {
			if err := db.RecordApproval(ctx, task.ID, "merge", quote); err != nil {
				return err
			}
		}
		if err := s.mergeWorkspaceMembers(ctx, db, project, cfg, task, intent, members); err != nil {
			_ = s.deliverNotices(ctx, db, project)
			return err
		}
		if members, err = s.workspaceMembers(ctx, db, project, task); err != nil {
			return err
		}
	}
	merged, err := db.SettleWorkspaceTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if err := s.deliverNotices(ctx, db, project); err != nil && !isHerdrUnavailable(err) {
		return err
	}
	current, err := db.TaskByID(ctx, project.ID, task.ID)
	if err != nil {
		return err
	}
	result := axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: string(current.State)}, {Key: "repos", Value: memberRows(members)}}
	if merged {
		if _, err := s.syncProjectRoot(ctx, db, project, cfg, true); err != nil {
			return err
		}
		if err := db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
			return err
		}
		if shouldAutoTeardownMerged(cfg) {
			teardown, err := s.teardownTask(ctx, db, project, cfg, current, false, "")
			if err != nil {
				return err
			}
			return out.Print(append(result, axi.Field{Key: "teardown", Value: "torn-down"}, axi.Field{Key: "branch_removed", Value: teardown.BranchRemoved}, axi.Field{Key: "help", Value: []any{"The Task Report remains in its Project record"}}))
		}
		if err := s.regenerateProjects(ctx, db); err != nil {
			return err
		}
		return out.Print(append(result, axi.Field{Key: "help", Value: []any{"Run `posse teardown " + taskIDString(task.Seq) + "` to release the Sandbox"}}))
	}
	if err := s.regenerateProjects(ctx, db); err != nil {
		return err
	}
	help := "Run `posse merge " + taskIDString(task.Seq) + " --execute --user-approved \"<User's words>\"` to merge every member"
	if merge {
		help = "Run `posse` or `posse watch` to observe the remaining pull requests"
	}
	return out.Print(append(result, axi.Field{Key: "help", Value: []any{help}}))
}

// openWorkspaceMerge gates every changed member and opens its merging: a
// recorded gated commit for local members, a pushed branch and pull request for
// pr members. The Task moves to merging once every changed member is ready.
func (s *Service) openWorkspaceMerge(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent, members []memberMerge) error {
	changed := 0
	var localReady []string
	for index := range members {
		member := &members[index]
		repo := member.repo
		if repo.State == store.TaskRepoMerged {
			changed++
			continue
		}
		ahead, err := commitsAhead(ctx, repo.WorktreePath, repo.BaseRef, "refs/heads/"+task.Branch)
		if err != nil {
			return memberFailure(repo.Repo, err)
		}
		if ahead == 0 {
			repo.State, repo.GatedSHA = store.TaskRepoUnchanged, ""
			if err := db.UpdateTaskRepo(ctx, repo); err != nil {
				return err
			}
			member.repo = repo
			continue
		}
		changed++
		if repo.MergeMode == "local" {
			if err := verifyFastForward(ctx, member.project, member.task); err != nil {
				return prefixFailure(repo.Repo, err)
			}
		} else if status, err := gitStatusOutput(ctx, repo.WorktreePath, "--porcelain"); err != nil {
			return memberFailure(repo.Repo, err)
		} else if status != "" {
			return axi.Failure("merge_refused", repo.Repo+": Task worktree is not clean", false, "Commit or discard the remaining changes, then signal done again")
		}
		gatedSHA, err := gitOutput(ctx, repo.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
		if err != nil {
			return memberFailure(repo.Repo, err)
		}
		gated := member.task
		gated.GatedSHA = gatedSHA
		if repo.MergeMode == "pr" {
			if err := s.requirePRTargetFresh(ctx, db, member.project, gated, repo.Repo, repo.MergeMode, gatedSHA); err != nil {
				return prefixFailure(repo.Repo, err)
			}
		}
		if repo.MergeMode == "pr" && repo.PRURL != "" {
			forge, err := forgeForRepository(ctx, member.target.Root, cfg, repo.Repo)
			if err != nil {
				return prefixFailure(repo.Repo, err)
			}
			if err := validateWorkerPullRequest(ctx, member.project, gated, forge, repo.PRURL, gatedSHA); err != nil {
				return prefixFailure(repo.Repo, err)
			}
		}
		if err := s.runIntentStep(ctx, db, intent, "gate.run:"+repo.Repo, func() error { return runMemberGates(ctx, memberGates(cfg, repo.Repo), repo.WorktreePath) }); err != nil {
			_, noticeErr := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "gate_failed", Summary: gated.Title + ": " + truncate(err.Error(), 240), DataJSON: marshalJSON(map[string]any{"repo": repo.Repo})})
			if noticeErr != nil {
				return noticeErr
			}
			return axi.Failure("gate_failed", repo.Repo+": "+err.Error(), false, "Fix the gate failure on the Task branch, then run `posse send "+taskIDString(task.Seq)+" <instruction>`")
		}
		if current, err := gitOutput(ctx, repo.WorktreePath, "rev-parse", "refs/heads/"+task.Branch); err != nil {
			return memberFailure(repo.Repo, err)
		} else if current != gatedSHA {
			return axi.Failure("branch_moved", repo.Repo+": Task branch moved while the Gate was running", false, "Run `posse merge "+taskIDString(task.Seq)+"` again to re-gate")
		}
		if repo.MergeMode == "pr" {
			if err := s.requirePRTargetFresh(ctx, db, member.project, member.task, repo.Repo, repo.MergeMode, gatedSHA); err != nil {
				return prefixFailure(repo.Repo, err)
			}
		}
		repo.GatedSHA = gatedSHA
		if repo.MergeMode == "local" {
			repo.State = store.TaskRepoGated
			localReady = append(localReady, repo.Repo)
		} else {
			forge, err := forgeForRepository(ctx, member.target.Root, cfg, repo.Repo)
			if err != nil {
				return prefixFailure(repo.Repo, err)
			}
			if err := validateForgeMergeMethod(forge, cfg); err != nil {
				return prefixFailure(repo.Repo, err)
			}
			created := repo.PRURL != ""
			if created {
				if err := validateWorkerPullRequest(ctx, member.project, gated, forge, repo.PRURL, gatedSHA); err != nil {
					return prefixFailure(repo.Repo, err)
				}
			} else {
				// Tasks completed before member publishing was available still Merge.
				branchRef := "refs/heads/" + task.Branch
				if err := s.runIntentStep(ctx, db, intent, "push:"+repo.Repo, func() error {
					_, pushErr := gitOutput(ctx, member.target.Root, "push", "origin", branchRef+":"+branchRef)
					return pushErr
				}); err != nil {
					return axi.Failure("pr_push_failed", repo.Repo+": could not push the gated Task branch", true, err.Error())
				}
				prURL, opened, err := s.findOrCreatePullRequest(ctx, db, member.project, gated, intent, forge, member.target.Name, "", "", "", "", false, "")
				if err != nil {
					return prefixFailure(repo.Repo, err)
				}
				repo.PRURL, created = prURL, opened
			}
			repo.State = store.TaskRepoMerge
			if created {
				if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "pr_opened", Summary: gated.Title + ": pull request opened", DataJSON: marshalJSON(map[string]any{"repo": repo.Repo, "url": repo.PRURL, "head_sha": gatedSHA})}); err != nil {
					return err
				}
			}
		}
		if err := db.UpdateTaskRepo(ctx, repo); err != nil {
			return err
		}
		member.repo = repo
	}
	if changed == 0 {
		return axi.Failure("merge_refused", "no member of "+taskIDString(task.Seq)+" has commits to Merge", false, "Send the Worker an instruction with `posse send`")
	}
	plan, planErr := task.AdmittedPlan()
	if planErr != nil {
		return admittedPlanFailure(planErr)
	}
	if !autoStartReviewLoop(plan) {
		if err := requireSelectedReview(ctx, db, project, cfg, task); err != nil {
			return err
		}
	}
	note := "Every changed member is gated or has an open pull request"
	if err := db.Transition(ctx, task.ID, store.StateDone, store.StateMerging, "cli", note); err != nil {
		return err
	}
	if len(localReady) > 0 {
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "merge_ready", Summary: task.Title + ": " + strings.Join(localReady, ", ") + " ready to merge locally", DataJSON: marshalJSON(map[string]any{"repos": localReady})}); err != nil {
			return err
		}
	}
	return nil
}

// mergeWorkspaceMembers merges each member that is ready: a local fast-forward to
// exactly the gated commit, or the forge merge of a pull request pinned to it.
func (s *Service) mergeWorkspaceMembers(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent, members []memberMerge) error {
	if err := requireSelectedReview(ctx, db, project, cfg, task); err != nil {
		return err
	}
	for _, member := range members {
		repo := member.repo
		switch repo.State {
		case store.TaskRepoGated:
			latest, err := gitOutput(ctx, repo.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
			if err != nil {
				return memberFailure(repo.Repo, err)
			}
			if latest != repo.GatedSHA {
				if err := s.reopenWorkspaceTask(ctx, db, task, repo, "Task branch in "+repo.Repo+" moved after the Gate passed"); err != nil {
					return err
				}
				return axi.Failure("branch_moved", repo.Repo+": Task branch moved after the Gate passed", false, "Run `posse merge "+taskIDString(task.Seq)+"` again to re-gate")
			}
			if err := s.runIntentStep(ctx, db, intent, "merge:"+repo.Repo, func() error {
				pathScope, err := localMergePathScope(ctx, member.project, member.task)
				if err != nil {
					return err
				}
				targetRef := "refs/heads/" + taskTargetBranch(member.project, member.task)
				return withIntentGitResources(ctx, db, project, task, intent.ID, member.target.Root, []string{targetRef}, pathScope,
					func() error {
						if err := requireSelectedReview(ctx, db, project, cfg, task); err != nil {
							return err
						}
						return mergeLocal(ctx, member.project, member.task)
					})
			}); err != nil {
				return prefixFailure(repo.Repo, err)
			}
			mergedRef, err := gitOutput(ctx, member.target.Root, "rev-parse", "refs/heads/"+taskTargetBranch(member.project, member.task))
			if err != nil {
				return memberFailure(repo.Repo, err)
			}
			repo.State, repo.MergedRef = store.TaskRepoMerged, mergedRef
			if err := db.UpdateTaskRepo(ctx, repo); err != nil {
				return err
			}
		case store.TaskRepoMerge:
			if err := s.mergeMemberPullRequest(ctx, db, project, cfg, task, intent, member); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) mergeMemberPullRequest(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent, member memberMerge) error {
	repo := member.repo
	if repo.GatedSHA == "" || repo.PRURL == "" {
		return axi.Failure("merge_refused", repo.Repo+": no recorded pull request or gated commit", false)
	}
	forge, err := forgeForRepository(ctx, member.target.Root, cfg, repo.Repo)
	if err != nil {
		return prefixFailure(repo.Repo, err)
	}
	if err := validateForgeMergeMethod(forge, cfg); err != nil {
		return prefixFailure(repo.Repo, err)
	}
	number, err := forgeReference(repo.PRURL, forge)
	if err != nil {
		return axi.Failure("pr_url_invalid", repo.Repo+": "+err.Error(), false)
	}
	var headSHA string
	if forge.Kind == "gitlab" {
		mr, viewErr := gitlabMR(ctx, forge, number)
		if viewErr != nil {
			return axi.Failure("pr_view_failed", repo.Repo+": could not verify the merge request head", true, viewErr.Error())
		}
		headSHA = mr.SHA
	} else {
		output, viewErr := commandOutputArgs(ctx, member.target.Root, "gh", "pr", "view", repo.PRURL, "--json", "headRefOid")
		if viewErr != nil {
			return axi.Failure("pr_view_failed", repo.Repo+": could not verify the pull request head", true, strings.TrimSpace(output))
		}
		var head struct {
			HeadRefOID string `json:"headRefOid"`
		}
		if err := json.Unmarshal([]byte(output), &head); err != nil || head.HeadRefOID == "" {
			return axi.Failure("pr_view_failed", repo.Repo+": gh pr view returned no headRefOid", true, strings.TrimSpace(output))
		}
		headSHA = head.HeadRefOID
	}
	if headSHA != repo.GatedSHA {
		if err := s.reopenWorkspaceTask(ctx, db, task, repo, "Pull request head in "+repo.Repo+" moved after the Gate passed"); err != nil {
			return err
		}
		return axi.Failure("branch_moved", repo.Repo+": pull request head moved after the Gate passed", false, "Run `posse merge "+taskIDString(task.Seq)+"` again to re-gate")
	}
	mergeLock, err := acquirePRMergeLock(ctx, member.target.Root)
	if err != nil {
		return prefixFailure(repo.Repo, err)
	}
	defer mergeLock.Close()
	if err := s.requirePRTargetFresh(ctx, db, member.project, member.task, repo.Repo, repo.MergeMode, repo.GatedSHA); err != nil {
		return prefixFailure(repo.Repo, err)
	}
	// Recheck the selected evidence immediately before each Member merge effect.
	if err := requireSelectedReview(ctx, db, project, cfg, task); err != nil {
		return prefixFailure(repo.Repo, err)
	}
	if err := s.requirePRTargetFresh(ctx, db, member.project, member.task, repo.Repo, repo.MergeMode, repo.GatedSHA); err != nil {
		return prefixFailure(repo.Repo, err)
	}
	method := defaultValue(cfg.Defaults.MergeMethod, "squash")
	if forge.Kind == "gitlab" {
		args := []string{"mr", "merge", fmt.Sprint(number), "--repo", "https://" + forge.Host + "/" + forge.Path, "--sha", repo.GatedSHA, "--yes", "--auto-merge=false"}
		if method == "squash" {
			args = append(args, "--squash")
		}
		if _, err := runOutputStep(ctx, db, intent, "pr.merge:"+repo.Repo, forge.Root, "glab", args...); err != nil {
			return axi.Failure("pr_merge_failed", repo.Repo+": GitLab did not merge the merge request", true, err.Error())
		}
	} else if _, err := runOutputStep(ctx, db, intent, "pr.merge:"+repo.Repo, member.target.Root, "gh", "pr", "merge", repo.PRURL, "--"+method, "--match-head-commit", repo.GatedSHA); err != nil {
		return axi.Failure("pr_merge_failed", repo.Repo+": GitHub did not merge the pull request", true, err.Error())
	}
	return nil
}

// reopenWorkspaceTask sends a merging Task back to done when one member's branch
// moved, so the next `posse merge` gates that member again.
func (s *Service) reopenWorkspaceTask(ctx context.Context, db *store.DB, task store.Task, repo store.TaskRepo, note string) error {
	repo.State, repo.GatedSHA = store.TaskRepoOpen, ""
	if err := db.UpdateTaskRepo(ctx, repo); err != nil {
		return err
	}
	current, err := db.TaskByID(ctx, task.ProjectID, task.ID)
	if err != nil {
		return err
	}
	if current.State != store.StateMerging {
		return nil
	}
	return db.Transition(ctx, task.ID, store.StateMerging, store.StateDone, "cli", note)
}

// recoverWorkspaceMergeIntent settles a workspace Merge that was interrupted: a
// gated local member whose commit already reached its frozen target is merged.
func (s *Service) recoverWorkspaceMergeIntent(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent) error {
	members, err := s.workspaceMembers(ctx, db, project, task)
	if err != nil {
		return err
	}
	for _, member := range members {
		repo := member.repo
		if repo.State != store.TaskRepoGated || repo.GatedSHA == "" {
			continue
		}
		targetRef := "refs/heads/" + taskTargetBranch(member.project, member.task)
		if _, err := gitOutput(ctx, member.target.Root, "merge-base", "--is-ancestor", repo.GatedSHA, targetRef); err != nil {
			continue
		}
		mergedRef, err := gitOutput(ctx, member.target.Root, "rev-parse", "refs/heads/"+taskTargetBranch(member.project, member.task))
		if err != nil {
			return err
		}
		repo.State, repo.MergedRef = store.TaskRepoMerged, mergedRef
		if err := db.UpdateTaskRepo(ctx, repo); err != nil {
			return err
		}
	}
	if _, err := db.SettleWorkspaceTask(ctx, task.ID); err != nil {
		return err
	}
	if err := db.ReconcileResourceLeasesForIntent(ctx, intent.ID, "re-read every gated Member target ref and settled the interrupted workspace Merge"); err != nil {
		return err
	}
	return db.FinishIntent(ctx, intent.ID, intent.ProcessID)
}

func runMemberGates(ctx context.Context, gates []string, worktree string) error {
	for _, gate := range gates {
		output, err := commandOutput(ctx, worktree, gate)
		if err != nil {
			return fmt.Errorf("gate %q failed: %s", gate, truncate(strings.TrimSpace(output), 1200))
		}
	}
	return nil
}

func memberRows(members []memberMerge) []any {
	rows := make([]any, 0, len(members))
	for _, member := range members {
		rows = append(rows, axi.Object{
			{Key: "repo", Value: member.repo.Repo}, {Key: "mode", Value: member.repo.MergeMode},
			{Key: "state", Value: member.repo.State}, {Key: "pr_url", Value: member.repo.PRURL},
		})
	}
	return rows
}

func memberFailure(repo string, err error) error {
	return fmt.Errorf("%s: %w", repo, err)
}

// prefixFailure names the member in a structured error's message.
func prefixFailure(repo string, err error) error {
	var failure *axi.Error
	if errors.As(err, &failure) {
		copied := *failure
		copied.Message = repo + ": " + copied.Message
		return &copied
	}
	return memberFailure(repo, err)
}

// pollWorkspacePullRequests watches every open member pull request of the
// Project's merging Tasks and records what changed.
func (s *Service) pollWorkspacePullRequests(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, force bool) error {
	return s.pollWorkspacePullRequestsAndNotify(ctx, db, project, cfg, force, nil)
}

func (s *Service) pollWorkspacePullRequestsAndNotify(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, force bool, onMerged func()) error {
	return s.pollWorkspacePullRequestsForRepo(ctx, db, project, cfg, force, onMerged, "", true, true, "")
}

func finishMemberPRPollOrRelease(ctx context.Context, db *store.DB, projectID int64, repo, token string, polledAt int64) error {
	if err := db.FinishMemberPRPoll(ctx, projectID, repo, token, polledAt); err != nil {
		return errors.Join(err, db.ReleaseMemberPRPoll(ctx, projectID, repo, token))
	}
	return nil
}

func (s *Service) pollWorkspacePullRequestsForRepo(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, force bool, onMerged func(), repoFilter string, claimProject, syncRoot bool, memberClaim string) error {
	now := time.Now()
	claim := ""
	var err error
	if claimProject {
		claim, err = db.ClaimPRPoll(ctx, project.ID, now, parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute), force)
		if err != nil {
			return err
		}
		if claim == "" {
			return nil
		}
		defer func() { _ = db.ReleasePRPoll(context.Background(), project.ID, claim) }()
	}
	if memberClaim != "" {
		defer func() {
			_ = finishMemberPRPollOrRelease(context.Background(), db, project.ID, repoFilter, memberClaim, time.Now().UnixMilli())
		}()
	}
	if claimProject || memberClaim != "" {
		if err := s.processPendingStackedTargetChanges(ctx, db, project, cfg, repoFilter); err != nil {
			return err
		}
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	type pollTarget struct {
		task     store.Task
		member   memberMerge
		members  []memberMerge
		previous store.PRObservation
		hasPrev  bool
	}
	type pollResult struct {
		target      pollTarget
		observation store.PRObservation
		checks      []failedCheck
		forge       repositoryForge
		err         error
	}
	type memberGroup struct {
		repo    string
		targets []pollTarget
	}
	groups := []memberGroup{}
	groupIndexes := map[string]int{}
	targetCount := 0
	for _, task := range tasks {
		if task.State == store.StateMerged || task.State == store.StateTornDown || task.State == store.StateReported {
			continue
		}
		members, err := s.workspaceMembers(ctx, db, project, task)
		if err != nil {
			return err
		}
		for _, member := range members {
			if repoFilter != "" && member.repo.Repo != repoFilter {
				continue
			}
			if member.repo.PRURL == "" || member.repo.State == store.TaskRepoMerged {
				continue
			}
			previous, previousErr := db.LatestMemberPRObservation(ctx, task.ID, member.repo.Repo)
			if previousErr != nil && !store.IsNotFound(previousErr) {
				return previousErr
			}
			if previousErr == nil && previous.PRURL == member.repo.PRURL && previous.State == "CLOSED" {
				if err := raiseClosedPRDecision(ctx, db, project, task, member.repo.PRURL); err != nil {
					return err
				}
			}
			target := pollTarget{task: task, member: member, members: append([]memberMerge(nil), members...), previous: previous, hasPrev: previousErr == nil && previous.PRURL == member.repo.PRURL}
			index, ok := groupIndexes[member.repo.Repo]
			if !ok {
				index = len(groups)
				groupIndexes[member.repo.Repo] = index
				groups = append(groups, memberGroup{repo: member.repo.Repo})
			}
			groups[index].targets = append(groups[index].targets, target)
			targetCount++
		}
	}

	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	results := make(chan pollResult, targetCount)
	var workers sync.WaitGroup
	for _, group := range groups {
		group := group
		workers.Add(1)
		go func() {
			defer workers.Done()
			claimToken := memberClaim
			if claimProject {
				var claimErr error
				claimToken, claimErr = db.ClaimMemberPRPoll(ctx, project.ID, group.repo, now, parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute), force)
				if claimErr != nil {
					for _, target := range group.targets {
						results <- pollResult{target: target, err: claimErr}
					}
					return
				}
				if claimToken == "" {
					return
				}
			}
			if claimToken != "" && claimProject {
				defer func() {
					if err := finishMemberPRPollOrRelease(context.Background(), db, project.ID, group.repo, claimToken, time.Now().UnixMilli()); err != nil {
						for _, target := range group.targets {
							results <- pollResult{target: target, err: err}
						}
					}
				}()
			}
			forge, forgeErr := forgeForRepository(pollCtx, group.targets[0].member.target.Root, cfg, group.repo)
			for _, target := range group.targets {
				member := target.member
				err := forgeErr
				var observation store.PRObservation
				var checks []failedCheck
				skipChecks := false
				if err == nil {
					headSHA := member.repo.GatedSHA
					if headSHA == "" && member.task.WorktreePath != "" && target.task.Branch != "" {
						headSHA, _ = gitOutput(pollCtx, member.task.WorktreePath, "rev-parse", "refs/heads/"+target.task.Branch)
					}
					freshness, freshnessErr := inspectPRTargetFreshness(pollCtx, member.target.Root, taskTargetBranch(member.project, member.task), headSHA)
					if freshnessErr != nil {
						err = freshnessErr
					} else if !freshness.Fresh {
						skipChecks = true
						err = queuePRTargetSyncInstruction(pollCtx, db, target.task, member.repo.Repo, member.repo.MergeMode, freshness, headSHA)
					}
				}
				if err == nil {
					if forge.Kind == "gitlab" {
						observation, checks, err = gitlabObservationWithChecks(pollCtx, forge, member.task, taskTargetBranch(member.project, member.task), now.UnixMilli(), !skipChecks)
					} else {
						var pull *ghPullRequest
						pull, err = viewMemberPullRequest(pollCtx, member, !skipChecks)
						if err == nil {
							observation, checks, err = makePRObservation(project.ID, member.task, pull, now, taskTargetBranch(member.project, member.task))
						}
					}
				}
				if err == nil && observation.State == "OPEN" {
					freshness, freshnessErr := inspectPRTargetFreshness(pollCtx, member.target.Root, taskTargetBranch(member.project, member.task), observation.HeadSHA)
					if freshnessErr != nil {
						err = freshnessErr
					} else if !freshness.Fresh {
						if queueErr := queuePRTargetSyncInstruction(pollCtx, db, target.task, member.repo.Repo, member.repo.MergeMode, freshness, observation.HeadSHA); queueErr != nil {
							err = queueErr
						} else {
							observation.Checks = stalePRChecks()
							checks = nil
						}
					}
				}
				results <- pollResult{target: target, observation: observation, checks: checks, forge: forge, err: err}
			}
		}()
	}
	go func() {
		workers.Wait()
		close(results)
	}()

	mergedAny := false
	mergedTargets := []pollTarget{}
	failures := []string{}
	var processingErr error
	for result := range results {
		if processingErr != nil {
			continue
		}
		task, member := result.target.task, result.target.member
		if result.err != nil {
			failures = append(failures, member.repo.Repo+": "+truncate(result.err.Error(), 240))
			if err := recordWorkspacePRTaskWatchFailure(ctx, db, project, member.task, member.repo.Repo, member.repo.PRURL, result.err, now); err != nil {
				processingErr = err
				cancelPoll()
			}
			continue
		}
		observation := result.observation
		previous, hasPrevious := result.target.previous, result.target.hasPrev
		if strings.EqualFold(observation.Mergeable, "UNKNOWN") && hasPrevious {
			observation.Mergeable = previous.Mergeable
		}
		effect := prObservationEffect(project, member.task, observation, result.checks, previous, hasPrevious)
		memberEffect := store.MemberPREffect{Notices: effect.Notices, MergedRef: effect.MergedRef}
		for index := range memberEffect.Notices {
			memberEffect.Notices[index].DataJSON = withRepo(memberEffect.Notices[index].DataJSON, member.repo.Repo)
		}
		if observation.State == "MERGED" {
			if err := settleUnchangedWorkspaceMembers(ctx, db, result.target.members); err != nil {
				processingErr = err
				cancelPoll()
				continue
			}
			branch, branchErr := gitOutput(ctx, member.task.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
			if (branchErr != nil || branch != task.Branch) && member.repo.GatedSHA != observation.HeadSHA {
				if err := recordWorkspacePRTaskWatchFailure(ctx, db, project, task, member.repo.Repo, member.repo.PRURL, fmt.Errorf("merged head is not on the Member's Task branch"), now); err != nil {
					processingErr = err
					cancelPoll()
				}
				continue
			}
			if _, err := gitOutput(ctx, member.task.WorktreePath, "merge-base", "--is-ancestor", observation.HeadSHA, "HEAD"); err != nil && member.repo.GatedSHA != observation.HeadSHA {
				if err := recordWorkspacePRTaskWatchFailure(ctx, db, project, task, member.repo.Repo, member.repo.PRURL, fmt.Errorf("merged head is not on the Member's Task branch"), now); err != nil {
					processingErr = err
					cancelPoll()
				}
				continue
			}
			memberEffect.RepoState = store.TaskRepoMerged
		}
		recorded, err := db.RecordMemberPRObservation(ctx, member.repo.Repo, observation, memberEffect)
		if err != nil {
			processingErr = err
			cancelPoll()
			continue
		}
		if observation.State == "OPEN" {
			if err := s.processCIObservation(ctx, db, project, member.task, observation, result.checks, result.forge, member.repo.Repo); err != nil {
				processingErr = err
				cancelPoll()
				continue
			}
		}
		if observation.State == "MERGED" {
			mergedTargets = append(mergedTargets, result.target)
		}
		if recorded && observation.State == "CLOSED" {
			if err := raiseClosedPRDecision(ctx, db, project, task, member.repo.PRURL); err != nil {
				processingErr = err
				cancelPoll()
				continue
			}
		}
		newlyMerged := recorded && memberEffect.RepoState == store.TaskRepoMerged
		if newlyMerged && syncRoot {
			if _, err := s.syncTaskTarget(ctx, db, member.project, member.task); err != nil {
				processingErr = err
				cancelPoll()
				continue
			}
		}
		mergedAny = mergedAny || newlyMerged
		if newlyMerged && onMerged != nil {
			onMerged()
		}
	}
	if processingErr != nil {
		return processingErr
	}
	for _, merged := range mergedTargets {
		if err := s.enqueueStackedTargetChanges(ctx, db, project, merged.member.task, merged.member.repo.Repo, taskTargetBranch(merged.member.project, merged.member.task)); err != nil {
			return err
		}
	}
	if claimProject || memberClaim != "" {
		if err := s.processPendingStackedTargetChanges(ctx, db, project, cfg, repoFilter); err != nil {
			return err
		}
	}
	if _, err := db.RecordPRPoll(ctx, project.ID, now.UnixMilli(), truncate(strings.Join(failures, "; "), 1000)); err != nil {
		return err
	}
	if mergedAny && syncRoot {
		if _, err := s.syncProjectRoot(ctx, db, project, cfg, true); err != nil {
			return err
		}
	}
	return nil
}

// An untouched member is only unchanged at settle time. A poll while the
// Worker is still working must not make that state permanent.
func settleUnchangedWorkspaceMembers(ctx context.Context, db *store.DB, members []memberMerge) error {
	for _, member := range members {
		if member.repo.PRURL != "" || (member.repo.State != store.TaskRepoOpen && member.repo.State != store.TaskRepoUnchanged) {
			continue
		}
		commits, err := gitOutput(ctx, member.task.WorktreePath, "rev-list", "--count", member.repo.BaseRef+"..HEAD")
		if err != nil {
			return memberFailure(member.repo.Repo, err)
		}
		state := store.TaskRepoOpen
		if commits == "0" {
			state = store.TaskRepoUnchanged
		}
		if member.repo.State != state {
			member.repo.State = state
			if err := db.UpdateTaskRepo(ctx, member.repo); err != nil {
				return err
			}
		}
	}
	return nil
}

// viewMemberPullRequest reads one member pull request with `gh pr view`, in the
// shape the GraphQL watch of a repository Project produces.
func viewMemberPullRequest(ctx context.Context, member memberMerge, includeChecks bool) (*ghPullRequest, error) {
	fields := "url,state,headRefOid,baseRefName,mergeable,reviewDecision,mergeCommit"
	if includeChecks {
		fields += ",statusCheckRollup"
	}
	output, err := commandOutputArgs(ctx, member.target.Root, "gh", "pr", "view", member.repo.PRURL, "--json", fields)
	if err != nil {
		return nil, fmt.Errorf("gh pr view: %w: %s", err, strings.TrimSpace(output))
	}
	var view struct {
		URL            string `json:"url"`
		State          string `json:"state"`
		HeadRefOID     string `json:"headRefOid"`
		BaseRefName    string `json:"baseRefName"`
		Mergeable      string `json:"mergeable"`
		ReviewDecision string `json:"reviewDecision"`
		MergeCommit    *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
		Checks []map[string]any `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(output), &view); err != nil {
		return nil, fmt.Errorf("invalid gh pr view output: %w", err)
	}
	pull := &ghPullRequest{URL: view.URL, State: view.State, HeadRefOID: view.HeadRefOID, BaseRefName: view.BaseRefName, Mergeable: view.Mergeable, ReviewDecision: view.ReviewDecision}
	if view.MergeCommit != nil {
		pull.MergeCommit = &struct {
			OID string `json:"oid"`
		}{OID: view.MergeCommit.OID}
	}
	if includeChecks {
		rollup := &ghStatusCheckRollup{State: rollupState(view.Checks)}
		rollup.Contexts.Nodes = view.Checks
		pull.StatusCheckRollup = rollup
	}
	return pull, nil
}

// rollupState folds individual checks into the GraphQL rollup state.
func rollupState(checks []map[string]any) string {
	if len(checks) == 0 {
		return "SUCCESS"
	}
	state := "SUCCESS"
	for _, check := range checks {
		conclusion, _ := check["conclusion"].(string)
		status, _ := check["status"].(string)
		contextState, _ := check["state"].(string)
		switch {
		case oneOfString(strings.ToUpper(conclusion), "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE"), oneOfString(strings.ToUpper(contextState), "ERROR", "FAILURE"):
			return "FAILURE"
		case status != "" && !strings.EqualFold(status, "COMPLETED"), strings.EqualFold(contextState, "PENDING"), strings.EqualFold(contextState, "EXPECTED"):
			state = "PENDING"
		}
	}
	return state
}

func withRepo(data, repo string) string {
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(data), &fields); err != nil || fields == nil {
		fields = map[string]any{}
	}
	fields["repo"] = repo
	return marshalJSON(fields)
}

// workspaceBranchTips records each member's Task branch tip as repo=sha pairs,
// so a discarded workspace Task stays recoverable by SHA in every member.
func (s *Service) workspaceBranchTips(ctx context.Context, db *store.DB, project store.Project, task store.Task) (string, error) {
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return "", err
	}
	tips := []string{}
	for _, repo := range repos {
		target, err := s.projectTarget(ctx, db, project, repo.Repo)
		if err != nil {
			continue
		}
		sha, err := gitOutput(ctx, target.Root, "rev-parse", "refs/heads/"+task.Branch)
		if err != nil {
			if isMissingGitRef(err) {
				continue
			}
			return "", err
		}
		tips = append(tips, repo.Repo+"="+sha)
	}
	return strings.Join(tips, ","), nil
}

// removeWorkspaceBranches deletes the Task branch in each member: after Merge
// only where it is merged into the member's frozen target branch; when discarding,
// exactly the tips recorded with the User's approval.
func (s *Service) removeWorkspaceBranches(ctx context.Context, db *store.DB, project store.Project, task store.Task, discardTips string) (bool, error) {
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return false, err
	}
	approved := map[string]string{}
	for _, pair := range strings.Split(discardTips, ",") {
		if name, sha, found := strings.Cut(pair, "="); found {
			approved[name] = sha
		}
	}
	ref := "refs/heads/" + task.Branch
	removed := false
	for _, repo := range repos {
		target, err := s.projectTarget(ctx, db, project, repo.Repo)
		if err != nil {
			continue
		}
		sha, revErr := gitOutput(ctx, target.Root, "rev-parse", ref)
		if revErr != nil {
			continue
		}
		if discardTips != "" {
			if approved[repo.Repo] != sha {
				continue
			}
		} else {
			memberProject := project
			memberProject.Root, memberProject.DefaultBranch, memberProject.Kind = target.Root, target.DefaultBranch, store.ProjectKindRepo
			memberTask := task
			memberTask.TargetBranch = repo.TargetBranch
			targetRef := "refs/heads/" + taskTargetBranch(memberProject, memberTask)
			if repo.MergeMode != "local" {
				var targetErr error
				targetRef, targetErr = taskTargetRef(ctx, memberProject, memberTask)
				if targetErr != nil {
					continue
				}
			}
			if _, mergeErr := gitOutput(ctx, target.Root, "merge-base", "--is-ancestor", ref, targetRef); mergeErr != nil {
				// Squash and rebase merges need not contain the branch ref. Delete
				// only the exact gated head that the recorded PR delivered.
				if repo.State != store.TaskRepoMerged || repo.MergeMode != "pr" || repo.MergedRef == "" || repo.GatedSHA != sha {
					continue
				}
			}
		}
		if _, err := gitOutput(ctx, target.Root, "update-ref", "-d", ref, sha); err != nil {
			return removed, memberFailure(repo.Repo, err)
		}
		removed = true
	}
	return removed, nil
}
