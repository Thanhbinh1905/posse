package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A workspace Task Lands member by member, each through its own Landing Mode, and
// the Task is landed only when every member with commits has Landed. The helpers
// of the single-repository path run on a per-member view of the Project and Task.

const workspaceLandIntent = "land --workspace"

type memberLanding struct {
	repo    store.TaskRepo
	target  repoTarget
	project store.Project // the Project seen as this member: its root and default branch
	task    store.Task    // the Task seen in this member: its worktree, gated commit and PR
}

func (s *Service) workspaceMembers(ctx context.Context, db *store.DB, project store.Project, task store.Task) ([]memberLanding, error) {
	repos, err := db.TaskRepos(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, axi.Failure("land_refused", "workspace Task has no member repositories", false)
	}
	members := make([]memberLanding, 0, len(repos))
	for _, repo := range repos {
		target, err := s.projectTarget(ctx, db, project, repo.Repo)
		if err != nil {
			return nil, err
		}
		view := project
		view.Root, view.DefaultBranch, view.Kind = target.Root, target.DefaultBranch, store.ProjectKindRepo
		memberTask := task
		memberTask.WorktreePath, memberTask.BaseRef, memberTask.LandingMode = repo.WorktreePath, repo.BaseRef, repo.LandingMode
		memberTask.GatedSHA, memberTask.PRURL, memberTask.LandedRef = repo.GatedSHA, repo.PRURL, repo.LandedRef
		memberTask.Title = repo.Repo + ": " + task.Title
		members = append(members, memberLanding{repo: repo, target: target, project: view, task: memberTask})
	}
	return members, nil
}

func memberGates(cfg config.Config, name string) []string {
	if repository, ok := cfg.Repositories[name]; ok && repository.Gate != nil {
		return repository.Gate
	}
	return cfg.Defaults.Gate
}

func (s *Service) landWorkspace(out *axi.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, merge bool, quote string) (returnErr error) {
	ctx := out.Context
	if task.State != store.StateDone && task.State != store.StateLanding {
		return axi.Failure("land_refused", "Task must be done or landing before Land", false, "Run `posse show "+taskIDString(task.Seq)+"` to inspect its state")
	}
	if merge && task.AutonomyLand != "auto" && quote == "" {
		return axi.Failure("land_approval_required", "merging a workspace Task needs User approval", false, "Pass `--user-approved \"<User's words>\"` after the User approves")
	}
	intent, err := s.startTaskIntent(ctx, db, project.ID, task.ID, workspaceLandIntent)
	if err != nil {
		return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	defer func() {
		if finishErr := db.FinishIntent(ctx, intent.ID, intent.ProcessID); returnErr == nil && finishErr != nil && !errors.Is(finishErr, store.ErrIntentOwned) {
			returnErr = finishErr
		}
	}()
	members, err := s.workspaceMembers(ctx, db, project, task)
	if err != nil {
		return err
	}
	if task.State == store.StateDone {
		if err := s.openWorkspaceLanding(ctx, db, project, cfg, task, intent, members); err != nil {
			_ = s.deliverNotices(ctx, db, project)
			return err
		}
		task.State = store.StateLanding
		if members, err = s.workspaceMembers(ctx, db, project, task); err != nil {
			return err
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
	landed, err := db.SettleWorkspaceTask(ctx, task.ID)
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
	if landed {
		if _, err := s.syncProjectRoot(ctx, db, project, cfg, true); err != nil {
			return err
		}
		if err := db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil {
			return err
		}
		if shouldAutoUnsaddleLanded(cfg) {
			teardown, err := s.unsaddleTask(ctx, db, project, cfg, current, false, "")
			if err != nil {
				return err
			}
			return out.Print(append(result, axi.Field{Key: "teardown", Value: "torn-down"}, axi.Field{Key: "branch_removed", Value: teardown.BranchRemoved}, axi.Field{Key: "help", Value: []any{"The Task Report remains in its Project record"}}))
		}
		if err := s.regenerateProjects(ctx, db); err != nil {
			return err
		}
		return out.Print(append(result, axi.Field{Key: "help", Value: []any{"Run `posse unsaddle " + taskIDString(task.Seq) + "` to release the Mount"}}))
	}
	if err := s.regenerateProjects(ctx, db); err != nil {
		return err
	}
	help := "Run `posse land " + taskIDString(task.Seq) + " --merge --user-approved \"<User's words>\"` to merge every member"
	if merge {
		help = "Run `posse` or `posse lookout` to observe the remaining pull requests"
	}
	return out.Print(append(result, axi.Field{Key: "help", Value: []any{help}}))
}

// openWorkspaceLanding gates every changed member and opens its landing: a
// recorded gated commit for local members, a pushed branch and pull request for
// pr members. The Task moves to landing once every changed member is ready.
func (s *Service) openWorkspaceLanding(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent, members []memberLanding) error {
	changed := 0
	var localReady []string
	for index := range members {
		member := &members[index]
		repo := member.repo
		if repo.State == store.TaskRepoLanded {
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
		if repo.LandingMode == "local" {
			if err := verifyFastForward(ctx, member.project, member.task); err != nil {
				return prefixFailure(repo.Repo, err)
			}
		} else if status, err := gitOutput(ctx, repo.WorktreePath, "status", "--porcelain"); err != nil {
			return memberFailure(repo.Repo, err)
		} else if status != "" {
			return axi.Failure("land_refused", repo.Repo+": Task worktree is not clean", false, "Commit or discard the remaining changes, then signal done again")
		}
		gatedSHA, err := gitOutput(ctx, repo.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
		if err != nil {
			return memberFailure(repo.Repo, err)
		}
		gated := member.task
		gated.GatedSHA = gatedSHA
		if repo.LandingMode == "pr" && repo.PRURL != "" {
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
			return axi.Failure("branch_moved", repo.Repo+": Task branch moved while the Gate was running", false, "Run `posse land "+taskIDString(task.Seq)+"` again to re-gate")
		}
		repo.GatedSHA = gatedSHA
		if repo.LandingMode == "local" {
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
				// Tasks completed before member publishing was available still Land.
				branchRef := "refs/heads/" + task.Branch
				if err := s.runIntentStep(ctx, db, intent, "push:"+repo.Repo, func() error {
					_, pushErr := gitOutput(ctx, member.target.Root, "push", "origin", branchRef+":"+branchRef)
					return pushErr
				}); err != nil {
					return axi.Failure("pr_push_failed", repo.Repo+": could not push the gated Task branch", true, err.Error())
				}
				prURL, opened, err := s.findOrCreatePullRequest(ctx, db, member.project, gated, intent, forge, member.target.Name, "", "", "", "")
				if err != nil {
					return prefixFailure(repo.Repo, err)
				}
				repo.PRURL, created = prURL, opened
			}
			repo.State = store.TaskRepoLanding
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
		return axi.Failure("land_refused", "no member of "+taskIDString(task.Seq)+" has commits to Land", false, "Send the Rider an instruction with `posse send`")
	}
	note := "Every changed member is gated or has an open pull request"
	if err := db.Transition(ctx, task.ID, store.StateDone, store.StateLanding, "cli", note); err != nil {
		return err
	}
	if len(localReady) > 0 {
		if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "land_ready", Summary: task.Title + ": " + strings.Join(localReady, ", ") + " ready to merge locally", DataJSON: marshalJSON(map[string]any{"repos": localReady})}); err != nil {
			return err
		}
	}
	return nil
}

// mergeWorkspaceMembers merges each member that is ready: a local fast-forward to
// exactly the gated commit, or the forge merge of a pull request pinned to it.
func (s *Service) mergeWorkspaceMembers(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, task store.Task, intent store.Intent, members []memberLanding) error {
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
				return axi.Failure("branch_moved", repo.Repo+": Task branch moved after the Gate passed", false, "Run `posse land "+taskIDString(task.Seq)+"` again to re-gate")
			}
			if err := s.runIntentStep(ctx, db, intent, "merge:"+repo.Repo, func() error { return mergeLocal(ctx, member.project, member.task) }); err != nil {
				return prefixFailure(repo.Repo, err)
			}
			landedRef, err := gitOutput(ctx, member.target.Root, "rev-parse", "refs/heads/"+member.target.DefaultBranch)
			if err != nil {
				return memberFailure(repo.Repo, err)
			}
			repo.State, repo.LandedRef = store.TaskRepoLanded, landedRef
			if err := db.UpdateTaskRepo(ctx, repo); err != nil {
				return err
			}
		case store.TaskRepoLanding:
			if err := s.mergeMemberPullRequest(ctx, db, cfg, task, intent, member); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) mergeMemberPullRequest(ctx context.Context, db *store.DB, cfg config.Config, task store.Task, intent store.Intent, member memberLanding) error {
	repo := member.repo
	if repo.GatedSHA == "" || repo.PRURL == "" {
		return axi.Failure("land_refused", repo.Repo+": no recorded pull request or gated commit", false)
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
		return axi.Failure("branch_moved", repo.Repo+": pull request head moved after the Gate passed", false, "Run `posse land "+taskIDString(task.Seq)+"` again to re-gate")
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

// reopenWorkspaceTask sends a landing Task back to done when one member's branch
// moved, so the next `posse land` gates that member again.
func (s *Service) reopenWorkspaceTask(ctx context.Context, db *store.DB, task store.Task, repo store.TaskRepo, note string) error {
	repo.State, repo.GatedSHA = store.TaskRepoOpen, ""
	if err := db.UpdateTaskRepo(ctx, repo); err != nil {
		return err
	}
	current, err := db.TaskByID(ctx, task.ProjectID, task.ID)
	if err != nil {
		return err
	}
	if current.State != store.StateLanding {
		return nil
	}
	return db.Transition(ctx, task.ID, store.StateLanding, store.StateDone, "cli", note)
}

// recoverWorkspaceLandIntent settles a workspace Land that was interrupted: a
// gated local member whose commit already reached its default branch is landed.
func (s *Service) recoverWorkspaceLandIntent(ctx context.Context, db *store.DB, project store.Project, task store.Task, intent store.Intent) error {
	members, err := s.workspaceMembers(ctx, db, project, task)
	if err != nil {
		return err
	}
	for _, member := range members {
		repo := member.repo
		if repo.State != store.TaskRepoGated || repo.GatedSHA == "" {
			continue
		}
		if _, err := gitOutput(ctx, member.target.Root, "merge-base", "--is-ancestor", repo.GatedSHA, "refs/heads/"+member.target.DefaultBranch); err != nil {
			continue
		}
		landedRef, err := gitOutput(ctx, member.target.Root, "rev-parse", "refs/heads/"+member.target.DefaultBranch)
		if err != nil {
			return err
		}
		repo.State, repo.LandedRef = store.TaskRepoLanded, landedRef
		if err := db.UpdateTaskRepo(ctx, repo); err != nil {
			return err
		}
	}
	if _, err := db.SettleWorkspaceTask(ctx, task.ID); err != nil {
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

func memberRows(members []memberLanding) []any {
	rows := make([]any, 0, len(members))
	for _, member := range members {
		rows = append(rows, axi.Object{
			{Key: "repo", Value: member.repo.Repo}, {Key: "mode", Value: member.repo.LandingMode},
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
// Project's landing Tasks and records what changed.
func (s *Service) pollWorkspacePullRequests(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, force bool) error {
	now := time.Now()
	claim, err := db.ClaimPRPoll(ctx, project.ID, now, parseDurationOr(cfg.Defaults.PRPoll, 2*time.Minute), force)
	if err != nil {
		return err
	}
	if claim == "" {
		return nil
	}
	defer func() { _ = db.ReleasePRPoll(context.Background(), project.ID, claim) }()
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	landedAny := false
	failure := ""
	for _, task := range tasks {
		if task.State == store.StateLanded || task.State == store.StateTornDown || task.State == store.StateReported {
			continue
		}
		members, err := s.workspaceMembers(ctx, db, project, task)
		if err != nil {
			return err
		}
		for _, member := range members {
			if member.repo.PRURL == "" || member.repo.State == store.TaskRepoLanded {
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
			forge, err := forgeForRepository(ctx, member.target.Root, cfg, member.repo.Repo)
			if err != nil {
				return prefixFailure(member.repo.Repo, err)
			}
			var observation store.PRObservation
			var failures []failedCheck
			if forge.Kind == "gitlab" {
				observation, failures, err = gitlabObservation(ctx, forge, member.task, now.UnixMilli())
			} else {
				var pull *ghPullRequest
				pull, err = viewMemberPullRequest(ctx, member)
				if err == nil {
					observation, failures, err = makePRObservation(project.ID, member.task, pull, now)
				}
			}
			if err != nil {
				failure = member.repo.Repo + ": " + truncate(err.Error(), 240)
				if noticeErr := recordPRTaskWatchFailure(ctx, db, project, member.task, err, now); noticeErr != nil {
					return noticeErr
				}
				continue
			}
			hasPrevious := previousErr == nil && previous.PRURL == member.repo.PRURL
			if strings.EqualFold(observation.Mergeable, "UNKNOWN") && hasPrevious {
				observation.Mergeable = previous.Mergeable
			}
			effect := prObservationEffect(project, member.task, observation, failures, previous, hasPrevious)
			memberEffect := store.MemberPREffect{Notices: effect.Notices, LandedRef: effect.LandedRef}
			for index := range memberEffect.Notices {
				memberEffect.Notices[index].DataJSON = withRepo(memberEffect.Notices[index].DataJSON, member.repo.Repo)
			}
			if observation.State == "MERGED" {
				if err := settleUnchangedWorkspaceMembers(ctx, db, members); err != nil {
					return err
				}
				branch, branchErr := gitOutput(ctx, member.task.WorktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
				if (branchErr != nil || branch != task.Branch) && member.repo.GatedSHA != observation.HeadSHA {
					if err := recordPRTaskWatchFailure(ctx, db, project, task, fmt.Errorf("%s: merged head is not on the Member's Task branch", member.repo.Repo), now); err != nil {
						return err
					}
					continue
				}
				if _, err := gitOutput(ctx, member.task.WorktreePath, "merge-base", "--is-ancestor", observation.HeadSHA, "HEAD"); err != nil && member.repo.GatedSHA != observation.HeadSHA {
					if err := recordPRTaskWatchFailure(ctx, db, project, task, fmt.Errorf("%s: merged head is not on the Member's Task branch", member.repo.Repo), now); err != nil {
						return err
					}
					continue
				}
				memberEffect.RepoState = store.TaskRepoLanded
			}
			recorded, err := db.RecordMemberPRObservation(ctx, member.repo.Repo, observation, memberEffect)
			if err != nil {
				return err
			}
			if recorded && observation.State == "CLOSED" {
				if err := raiseClosedPRDecision(ctx, db, project, task, member.repo.PRURL); err != nil {
					return err
				}
			}
			landedAny = landedAny || (recorded && memberEffect.RepoState == store.TaskRepoLanded)
		}
	}
	if _, err := db.RecordPRPoll(ctx, project.ID, now.UnixMilli(), failure); err != nil {
		return err
	}
	if landedAny {
		if _, err := s.syncProjectRoot(ctx, db, project, cfg, true); err != nil {
			return err
		}
	}
	return nil
}

// An untouched member is only unchanged at settle time. A poll while the
// Rider is still working must not make that state permanent.
func settleUnchangedWorkspaceMembers(ctx context.Context, db *store.DB, members []memberLanding) error {
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
func viewMemberPullRequest(ctx context.Context, member memberLanding) (*ghPullRequest, error) {
	output, err := commandOutputArgs(ctx, member.target.Root, "gh", "pr", "view", member.repo.PRURL, "--json", "url,state,headRefOid,mergeable,reviewDecision,mergeCommit,statusCheckRollup")
	if err != nil {
		return nil, fmt.Errorf("gh pr view: %w: %s", err, strings.TrimSpace(output))
	}
	var view struct {
		URL            string `json:"url"`
		State          string `json:"state"`
		HeadRefOID     string `json:"headRefOid"`
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
	pull := &ghPullRequest{URL: view.URL, State: view.State, HeadRefOID: view.HeadRefOID, Mergeable: view.Mergeable, ReviewDecision: view.ReviewDecision}
	if view.MergeCommit != nil {
		pull.MergeCommit = &struct {
			OID string `json:"oid"`
		}{OID: view.MergeCommit.OID}
	}
	rollup := &ghStatusCheckRollup{State: rollupState(view.Checks)}
	rollup.Contexts.Nodes = view.Checks
	pull.StatusCheckRollup = rollup
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

// removeWorkspaceBranches deletes the Task branch in each member: after Landing
// only where it is merged into the member's default branch; when discarding,
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
		} else if _, mergeErr := gitOutput(ctx, target.Root, "merge-base", "--is-ancestor", ref, "refs/heads/"+target.DefaultBranch); mergeErr != nil {
			continue
		}
		if _, err := gitOutput(ctx, target.Root, "update-ref", "-d", ref, sha); err != nil {
			return removed, memberFailure(repo.Repo, err)
		}
		removed = true
	}
	return removed, nil
}
