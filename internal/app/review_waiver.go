package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) reviewWaive(out *axi.Context, args []string) error {
	parsed, err := parseArgs("review waive", args, map[string]flagSpec{"head": {}, "repo": {}, "user-approved": {}})
	if err != nil {
		return err
	}
	head, repo, quote := parsed.Flags["head"], parsed.Flags["repo"], parsed.Flags["user-approved"]
	if len(parsed.Positionals) != 1 || !validGitObjectID(head) {
		return axi.Usage("review waive requires a Task and --head <full commit SHA>")
	}
	trimmed := strings.TrimSpace(quote)
	if trimmed == "" {
		return axi.Failure("user_approval_required", "waiving review requires explicit approval for this exact head", false, "Pass --user-approved with the User's exact words")
	}
	if strings.HasPrefix(trimmed, "<") && strings.HasSuffix(trimmed, ">") {
		return axi.Failure("user_approval_placeholder", "--user-approved requires the User's actual words, not a placeholder", false)
	}
	ctx := out.Context
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx, db, project); err != nil {
		return err
	}
	task, err := db.Task(ctx, project.ID, parsed.Positionals[0])
	if err != nil {
		return err
	}
	switch task.State {
	case store.StateWorking, store.StateStalled, store.StateNeedsDecision, store.StateDone, store.StateMerging:
	default:
		return axi.Failure("review_waiver_refused", "only an active Change Task can waive review", false)
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	if task.Type != "change" || plan == nil || task.Branch == "" {
		return axi.Failure("review_waiver_refused", "only an admitted Change Task with its own branch can waive review", false)
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	lane := task
	root, repository := project.Root, project.Name
	if project.IsWorkspace() {
		members, err := workspaceMembers(ctx, db, project, task)
		if err != nil {
			return err
		}
		found := false
		for _, member := range members {
			if member.repo.Repo == repo && member.repo.State != store.TaskRepoMerged {
				lane, root, repository = member.task, member.target.Root, repo
				found = true
				break
			}
		}
		if !found {
			return axi.Failure("review_waiver_refused", "specify a Task Member that has not merged with --repo", false)
		}
	} else if repo != "" {
		return axi.Usage("review waive --repo is only valid for a workspace Task")
	}
	if lane.MergeMode != "pr" || !greenhouseFullPipelineTaskWithConfig(task, cfg, repo) {
		return axi.Failure("review_waiver_refused", "only Greenhouse full-pipeline PR delivery supports CI-only waivers", false)
	}
	lock, err := acquireTaskBranchAuthorLock(ctx, root, task.Branch)
	if err != nil {
		return err
	}
	defer lock.Close()
	ref := "refs/heads/" + task.Branch
	current, err := taskBranchAuthorHead(ctx, root, task.Branch)
	if err != nil {
		return err
	}
	if current != head {
		return axi.Failure("review_waiver_head_mismatch", "approved head does not match the current Task branch head", false, "Current head: "+current)
	}
	ahead, err := commitsAhead(ctx, root, lane.BaseRef, ref)
	if err != nil {
		return err
	}
	if ahead == 0 {
		return axi.Failure("review_waiver_refused", "Task branch has no commits ahead of its base", false)
	}
	id, err := db.CreateNotice(ctx, store.Notice{
		ProjectID: project.ID, TaskID: task.ID, Kind: "review_waived",
		Summary:  fmt.Sprintf("%s: User waived Greenhouse review for %s at %s; forge CI is still required", task.Title, repository, shortSHA(head)),
		DataJSON: marshalJSON(map[string]string{"repository": repository, "head": head, "user_quote": quote}),
	})
	if err != nil {
		return err
	}
	return out.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)}, {Key: "repository", Value: repository}, {Key: "head", Value: head}, {Key: "notice", Value: id},
		{Key: "help", Value: []any{"Have the Worker publish the approved head unchanged; Merge still requires green forge CI and its own authorization"}},
	})
}

func hasReviewWaiver(ctx context.Context, db *store.DB, taskID int64, repository, head string) (bool, error) {
	var waived bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notices WHERE task_id=? AND kind='review_waived'
AND json_extract(data_json,'$.repository')=? AND json_extract(data_json,'$.head')=? AND length(trim(json_extract(data_json,'$.user_quote')))>0)`, taskID, repository, head).Scan(&waived)
	return waived, err
}
