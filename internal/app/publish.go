package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func remoteBranchHead(ctx context.Context, root, ref string) (string, error) {
	output, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return "", fmt.Errorf("unexpected git ls-remote output %q", line)
		}
		if fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", nil
}

// publish is the only supported direct delivery path for a normal PR-mode Worker.
// It never accepts a branch, remote, refspec, or merge option from the caller.
func (s *Service) publish(out *axi.Context, args []string) (returnErr error) {
	parsed, err := parseArgs("publish", args, map[string]flagSpec{"repo": {}, "verify": {repeatable: true}, "proof": {repeatable: true}, "risk": {repeatable: true}, "refresh": {boolean: true}, "rewrite": {boolean: true}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || strings.TrimSpace(parsed.Positionals[0]) == "" {
		return axi.Usage("publish requires a Worker summary")
	}
	rewrite, rewriteQuote := parsed.Bool("rewrite"), parsed.Flags["user-approved"]
	if rewrite != (strings.TrimSpace(rewriteQuote) != "") {
		return axi.Usage("publish --rewrite requires --user-approved \"<User's words>\", and --user-approved requires --rewrite")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if !s.workerCaller(out.Context, home) {
		return axi.Failure("worker_only", "publish must run from a Worker", false)
	}
	task, err := s.workerTask(out.Context, db)
	if err != nil {
		return axi.Failure("worker_task_unknown", "cannot find the Worker's Task", false)
	}
	project, err := db.ProjectByID(out.Context, task.ProjectID)
	if err != nil {
		return err
	}
	plan, planErr := task.AdmittedPlan()
	if planErr != nil {
		return admittedPlanFailure(planErr)
	}
	if plan != nil && plan.Execution.Mode == "in-place" {
		return axi.Failure("publish_refused", "in-place Tasks cannot publish or open a PR", false)
	}
	cfg, err := s.prepareProject(out.Context, db, project)
	if err != nil {
		return err
	}
	repoFilter := ""
	if project.IsWorkspace() {
		if rewrite {
			return axi.Failure("publish_refused", "--rewrite supports single-repository Tasks only", false)
		}
		if parsed.Flags["repo"] == "" {
			return axi.Failure("publish_refused", "workspace publishing requires --repo <member>", false)
		}
		repoFilter = parsed.Flags["repo"]
	}
	if !isFullPipelineTask(task, repoFilter) {
		if err := s.processPendingStackedTargetChanges(out.Context, db, project, cfg, repoFilter); err != nil {
			return err
		}
	}
	task, err = db.TaskByID(out.Context, project.ID, task.ID)
	if err != nil {
		return err
	}
	changes, err := db.TaskTargetChanges(out.Context, task.ID)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if change.Repo == repoFilter && change.Status != "complete" {
			return axi.Failure("stack_retarget_incomplete", "stacked PR/MR target change is "+change.Status+": "+change.Error, change.Status == "pending", "Resolve the stacked PR/MR target change before publishing again")
		}
	}
	if task.Type != "change" || (task.State != store.StateWorking && task.State != store.StateStalled) || task.WorktreePath == "" || task.Branch == "" {
		return axi.Failure("publish_refused", "only a working PR-mode Change Worker with its own Task branch can publish", false)
	}
	cwd, err := currentDir()
	if err != nil {
		return axi.Failure("publish_refused", "could not resolve the current directory", false)
	}
	if isFullPipelineTask(task, repoFilter) {
		if rewrite {
			return axi.Failure("publish_refused", "--rewrite does not support full-pipeline Tasks", false)
		}
		return s.publishFullPipeline(out, db, home, project, task, repoFilter, cwd, cfg, parsed)
	}
	if !pathWithin(task.WorktreePath, cwd) {
		return axi.Failure("publish_refused", "publish must run inside this Worker's Task Sandbox", false)
	}
	var member *memberMerge
	if project.IsWorkspace() {
		members, err := workspaceMembers(out.Context, db, project, task)
		if err != nil {
			return err
		}
		for i := range members {
			if members[i].repo.Repo == parsed.Flags["repo"] {
				member = &members[i]
				break
			}
		}
		if member == nil || member.repo.MergeMode != "pr" {
			return axi.Failure("publish_refused", "only a PR-mode member named in this Task may be published", false)
		}
		project, task = member.project, member.task
	} else {
		if parsed.Flags["repo"] != "" || task.MergeMode != "pr" {
			return axi.Failure("publish_refused", "only a PR-mode Change Task may be published without --repo", false)
		}
		root, err := gitTop(out.Context, "")
		if err != nil || !samePath(root, task.WorktreePath) {
			return axi.Failure("publish_refused", "publish must run at the Task Sandbox", false)
		}
	}
	if err := validateChangeSignal(out.Context, task); err != nil {
		return axi.Failure("publish_refused", err.Error(), false)
	}
	branch, err := gitOutput(out.Context, task.WorktreePath, "symbolic-ref", "HEAD")
	if err != nil || branch != "refs/heads/"+task.Branch {
		return axi.Failure("publish_refused", "Sandbox must be checked out on its Task branch", false)
	}
	sha, err := gitOutput(out.Context, task.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	branchHead, err := gitOutput(out.Context, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil {
		return err
	}
	if branchHead != sha {
		return axi.Failure("publish_refused", "Task branch moved while publish was inspecting the Sandbox", true, "Retry `posse publish` after the branch settles")
	}
	if err := refuseAgentAttribution(out.Context, task.WorktreePath, sha, task); err != nil {
		return err
	}
	forge, err := forgeForRepository(out.Context, project.Root, cfg, parsed.Flags["repo"])
	if err != nil {
		return err
	}
	if err := validateForgeMergeMethod(forge, cfg); err != nil {
		return err
	}
	intent, err := s.startTaskIntent(out.Context, db, project.ID, task.ID, "publish")
	if err != nil {
		return axi.Failure("intent_active", "Task already has an unfinished command", true, err.Error())
	}
	defer func() {
		if err := db.FinishIntent(out.Context, intent.ID, intent.ProcessID); returnErr == nil && err != nil {
			returnErr = err
		}
	}()
	ref := "refs/heads/" + task.Branch
	previousRemoteHead, err := remoteBranchHead(out.Context, task.WorktreePath, ref)
	if err != nil {
		return axi.Failure("pr_remote_head_failed", "could not inspect the Task branch on origin before publishing", true, err.Error())
	}
	var pushOptions []string
	if publishRewritesHistory(out.Context, task.WorktreePath, previousRemoteHead, sha) != rewrite {
		if rewrite {
			return axi.Failure("rewrite_not_needed", "publishing this head rewrites no published history", false, "Run `posse publish \"<summary>\"` without --rewrite")
		}
		return axi.Failure("publish_rewrite_required", "the local Task commit does not descend from the Task branch on origin; publishing would rewrite published history", false,
			"If the User approved rewriting this Task's published history, run `posse publish --rewrite --user-approved \"<User's words>\" \"<summary>\"`",
			"Otherwise restore the published commits into the Task branch and publish again")
	}
	if rewrite {
		published, err := db.WasVerifiedTaskHead(out.Context, task.ID, previousRemoteHead)
		if err != nil {
			return err
		}
		if !published {
			return axi.Failure("publish_rewrite_refused", "the Task branch on origin is not the head Posse last published for this Task", false, "Ask the Lead to resolve the unknown commits on origin before rewriting")
		}
		pushOptions = append(pushOptions, "--force-with-lease="+ref+":"+previousRemoteHead)
	}
	if err := db.RecordPublishPrePushHead(out.Context, task.ID, parsed.Flags["repo"], previousRemoteHead); err != nil {
		return err
	}
	if err := pushWithCredentialRetry(out.Context, task.WorktreePath, ref, pushOptions...); err != nil {
		return axi.Failure("pr_push_failed", "could not push the Task branch", true, pushFailureHelp(out.Context, task.WorktreePath, err)...)
	}
	if rewrite {
		if err := recordBranchRewrite(out.Context, db, project, task, parsed.Flags["repo"], previousRemoteHead, sha, rewriteQuote); err != nil {
			return err
		}
	}
	// findOrCreate uses the expected tip to reject an existing PR on a moved head.
	task.GatedSHA = sha // ephemeral until the Lead records the merge head.
	verification := strings.Join(parsed.Repeated["verify"], "\n")
	proof := strings.Join(parsed.Repeated["proof"], "\n\n")
	risk := strings.Join(parsed.Repeated["risk"], "\n\n")
	prURL, _, err := s.findOrCreatePullRequest(out.Context, db, project, task, intent, forge, parsed.Flags["repo"], parsed.Positionals[0], verification, proof, risk, parsed.Bool("refresh"), previousRemoteHead)
	if err != nil {
		return err
	}
	if err := validateWorkerPullRequest(out.Context, project, task, forge, prURL, sha); err != nil {
		return err
	}
	if member == nil {
		if err := db.RecordVerifiedPRHead(out.Context, task.ID, prURL, sha); err != nil {
			return err
		}
		if err := createPROpenedNotice(out.Context, db, project, task, prURL, sha, task.Title+": pull request opened"); err != nil {
			return err
		}
	}
	help := "Run `posse signal done \"<summary>\" --pr " + prURL + "` after committing and ensuring a clean worktree"
	result := axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "pr_url", Value: prURL}}
	if member != nil {
		member.repo.PRURL = prURL
		if err := db.UpdateTaskRepo(out.Context, member.repo); err != nil {
			return err
		}
		help = "Publish every changed PR member, then run `posse signal done \"<summary>\"`"
		result = append(result, axi.Field{Key: "repo", Value: member.repo.Repo})
	}
	return out.Print(append(result, axi.Field{Key: "help", Value: []any{help}}))
}

// publishRewritesHistory reports whether pushing head would drop commits
// already on origin, including a remote head missing locally.
func publishRewritesHistory(ctx context.Context, worktree, remoteHead, head string) bool {
	return remoteHead != "" && !gitAncestor(ctx, worktree, remoteHead, head)
}

// publishFullPipeline is the full-pipeline entry point of publish for one
// lane. From the author Sandbox it submits a private candidate; from that
// candidate's checkout it promotes the fully validated head. PR and local
// delivery, single-repository and per-Member, share this path.
func (s *Service) publishFullPipeline(out *axi.Context, db *store.DB, home string, project store.Project, task store.Task, repo, cwd string, cfg config.Config, parsed parsedArgs) error {
	ctx := out.Context
	lane, err := resolveCandidateLane(ctx, db, project, task, repo)
	if err != nil {
		return err
	}
	if lane.task.MergeMode != "pr" && !lane.localDelivery() {
		return axi.Failure("publish_refused", lane.memberLabel("full-pipeline publication supports PR and local delivery only"), false)
	}
	candidate, candidateErr := db.LatestValidationCandidate(ctx, task.ID, repo)
	candidateCWD := candidateErr == nil && pathWithin(candidate.WorktreePath, cwd)
	if candidateCWD {
		root, err := gitTop(ctx, "")
		if err != nil || !samePath(root, candidate.WorktreePath) {
			return axi.Failure("publish_refused", "publish must run at the root of the private candidate checkout", false)
		}
	} else if project.IsWorkspace() {
		if !pathWithin(task.WorktreePath, cwd) {
			return axi.Failure("publish_refused", "publish must run inside this Worker's Task Sandbox or the Member's private candidate checkout", false)
		}
	} else if root, err := gitTop(ctx, ""); err != nil || !samePath(root, task.WorktreePath) {
		return axi.Failure("publish_refused", "publish must run at the Task Sandbox or its private candidate checkout", false)
	}
	if err := s.refuseMovedCandidateAuthor(ctx, db, project, task, true); err != nil {
		return err
	}
	if err := validateChangeSignal(ctx, lane.task); err != nil {
		return axi.Failure("publish_refused", lane.memberLabel(err.Error()), false)
	}
	head, branch, err := authorBranchHead(ctx, lane.task)
	if err != nil || branch != "refs/heads/"+task.Branch {
		return axi.Failure("publish_refused", lane.memberLabel("Sandbox must be checked out on its Task branch"), false)
	}
	if branchHead, err := gitOutput(ctx, lane.task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch); err != nil {
		return err
	} else if branchHead != head {
		return axi.Failure("publish_refused", "Task branch moved while publish was inspecting the Sandbox", true, "Retry `posse publish` after the branch settles")
	}
	if err := refuseAgentAttribution(ctx, lane.task.WorktreePath, head, lane.task); err != nil {
		return err
	}
	if candidateCWD {
		if err := s.ensureCandidateCheckout(ctx, db, home, lane.task, candidate); err != nil {
			return err
		}
		return s.promotePrivateCandidate(out, db, home, lane, cfg)
	}
	return s.submitPrivateCandidate(out, db, home, lane, parsed.Positionals[0], strings.Join(parsed.Repeated["verify"], "\n"), strings.Join(parsed.Repeated["proof"], "\n\n"), strings.Join(parsed.Repeated["risk"], "\n\n"), parsed.Bool("refresh"))
}

// A workspace Signal must account for every changed PR-mode member, not just
// one URL in the Task row. Local and unchanged members need no PR. A
// full-pipeline Member answering a Review fix or requesting Review from its
// private candidate has not been published yet.
func (s *Service) validateWorkspacePublishedPRs(ctx context.Context, db *store.DB, project store.Project, task store.Task, cfg config.Config, candidateFix bool) error {
	members, err := workspaceMembers(ctx, db, project, task)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.repo.MergeMode != "pr" || candidateFix && isFullPipelineTask(task, member.repo.Repo) {
			continue
		}
		ahead, err := commitsAhead(ctx, member.repo.WorktreePath, member.repo.BaseRef, "refs/heads/"+task.Branch)
		if err != nil {
			return memberFailure(member.repo.Repo, err)
		}
		if ahead == 0 {
			continue
		}
		if member.repo.PRURL == "" {
			return axi.Failure("signal_invalid", member.repo.Repo+": publish its PR before signalling done", false, "Run `posse publish --repo "+member.repo.Repo+" \"<summary>\" [--verify \"<command> -> <result>\"] [--proof \"<markdown>\"] [--risk \"<markdown>\"]`")
		}
		sha, err := gitOutput(ctx, member.repo.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
		if err != nil {
			return memberFailure(member.repo.Repo, err)
		}
		forge, err := forgeForRepository(ctx, member.target.Root, cfg, member.repo.Repo)
		if err != nil {
			return prefixFailure(member.repo.Repo, err)
		}
		if err := validateWorkerPullRequest(ctx, member.project, member.task, forge, member.repo.PRURL, sha); err != nil {
			return prefixFailure(member.repo.Repo, err)
		}
		s.refreshFailedObservedPRChecks(ctx, db, project, cfg, task.ID, member.repo.Repo, member.repo.PRURL, sha)
		if err := rejectFailedObservedPRChecks(ctx, db, task.ID, member.repo.Repo, member.repo.PRURL, sha); err != nil {
			return prefixFailure(member.repo.Repo, err)
		}
	}
	return nil
}

// Check the remote branch as well as the forge's source repository, branch,
// target, URL, state and head. A matching commit from a fork is not sufficient.
func validateWorkerPullRequest(ctx context.Context, project store.Project, task store.Task, forge repositoryForge, prURL, sha string) error {
	return validateWorkerPullRequestWithRemote(ctx, project, task, forge, prURL, sha, true)
}

func validateWorkerPullRequestWithRemote(ctx context.Context, project store.Project, task store.Task, forge repositoryForge, prURL, sha string, checkRemote bool) error {
	number, err := forgeReference(prURL, forge)
	if err != nil {
		return axi.Failure("pr_url_invalid", "pull request is not in the Project repository", false, err.Error())
	}
	if task.Branch == "" || task.WorktreePath == "" {
		return axi.Failure("pr_branch_mismatch", "Task has no own branch", false)
	}
	if forge.Kind == "gitlab" {
		mr, err := gitlabMR(ctx, forge, number)
		if err != nil {
			return axi.Failure("pr_view_failed", "could not verify the merge request", true, err.Error())
		}
		if mr.WebURL != prURL || (mr.State != "opened" && mr.State != "merged") || mr.SHA != sha || mr.SourceBranch != task.Branch || mr.TargetBranch != taskTargetBranch(project, task) || mr.SourceProjectID == 0 || mr.SourceProjectID != mr.TargetProjectID {
			return axi.Failure("pr_head_mismatch", "merge request does not target this Project from its Task branch and commit", false)
		}
		if !checkRemote {
			return nil
		}
		return validateWorkerRemoteBranch(ctx, task, sha, mr.State == "merged")
	}
	output, err := commandOutputArgs(ctx, project.Root, "gh", "pr", "view", prURL, "--json", "url,state,headRefOid,headRefName,baseRefName,headRepository")
	if err != nil {
		return axi.Failure("pr_view_failed", "could not verify the pull request", true, strings.TrimSpace(output))
	}
	var pull struct {
		URL            string `json:"url"`
		State          string `json:"state"`
		HeadRefOID     string `json:"headRefOid"`
		HeadRefName    string `json:"headRefName"`
		BaseRefName    string `json:"baseRefName"`
		HeadRepository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"headRepository"`
	}
	if err := json.Unmarshal([]byte(output), &pull); err != nil {
		return axi.Failure("pr_view_failed", "gh returned invalid pull request data", true, err.Error())
	}
	if pull.URL != prURL || (pull.State != "OPEN" && pull.State != "MERGED") || pull.HeadRefOID != sha || pull.HeadRefName != task.Branch || pull.BaseRefName != taskTargetBranch(project, task) || !strings.EqualFold(pull.HeadRepository.NameWithOwner, forge.Path) {
		return axi.Failure("pr_head_mismatch", fmt.Sprintf("pull request does not target %s from its Task branch and commit", forge.Path), false)
	}
	if !checkRemote {
		return nil
	}
	return validateWorkerRemoteBranch(ctx, task, sha, pull.State == "MERGED")
}

func validateWorkerRemoteBranch(ctx context.Context, task store.Task, sha string, allowDeleted bool) error {
	ref := "refs/heads/" + task.Branch
	remote, err := gitOutput(ctx, task.WorktreePath, "ls-remote", "origin", ref)
	if err != nil {
		return axi.Failure("pr_head_unavailable", "could not verify the pushed Task branch", true, err.Error())
	}
	if remote == sha+"\t"+ref || (allowDeleted && remote == "") {
		return nil
	}
	return axi.Failure("pr_head_mismatch", "origin Task branch differs from the local Task commit", false)
}

// pushCredentialRetries covers a credential helper that transiently returns no
// credentials, which git reports as a missing HTTPS username.
var pushCredentialRetries = 2

var pushCredentialRetryDelay = 500 * time.Millisecond

func isCredentialPromptFailure(err error) bool {
	message := err.Error()
	return strings.Contains(message, "could not read Username") || strings.Contains(message, "could not read Password")
}

func pushWithCredentialRetry(ctx context.Context, worktree, ref string, options ...string) error {
	args := append(append([]string{"push"}, options...), "origin", ref+":"+ref)
	var err error
	for attempt := 0; attempt <= pushCredentialRetries; attempt++ {
		if _, err = gitOutput(ctx, worktree, args...); err == nil || !isCredentialPromptFailure(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pushCredentialRetryDelay):
		}
	}
	return err
}

// pushFailureHelp names the credential helper when git could not get HTTPS credentials.
func pushFailureHelp(ctx context.Context, worktree string, pushErr error) []string {
	help := []string{pushErr.Error()}
	if !isCredentialPromptFailure(pushErr) {
		return help
	}
	helper, err := gitOutput(ctx, worktree, "config", "--get-urlmatch", "credential.helper", "https://github.com")
	if err != nil || helper == "" {
		return append(help, "git has no credential helper for https://github.com; configure one such as `gh auth setup-git`")
	}
	return append(help, fmt.Sprintf("git credential helper %q returned no credentials; run `gh auth status` and `printf 'protocol=https\\nhost=github.com\\n\\n' | git credential fill` in this pane, then retry `posse publish`", helper))
}
