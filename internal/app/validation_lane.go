package app

import (
	"context"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// candidateLane is one repository through which a full-pipeline candidate is
// delivered: the Task's own repository, or one workspace Member seen as its
// own repository. Custody, ordered validation and the publication barrier
// resolve a lane first, so workspace Members and local delivery reuse the
// single-repository candidate machinery instead of parallel paths.
type candidateLane struct {
	repo      string        // workspace Member name; "" for a single-repository Task
	owner     store.Project // the Task's Project, a workspace for a Member lane
	ownerTask store.Task    // the Task as stored; Review evidence covers all its lanes
	project   store.Project // the lane's repository: its root and default branch
	task      store.Task    // the Task seen in the lane: worktree, target, Merge Mode and PR
	member    store.TaskRepo
}

func resolveCandidateLane(ctx context.Context, db *store.DB, project store.Project, task store.Task, repo string) (candidateLane, error) {
	if !project.IsWorkspace() {
		if repo != "" {
			return candidateLane{}, axi.Failure("validation_repo_invalid", "--repo is only valid for a workspace Task", false)
		}
		return candidateLane{owner: project, ownerTask: task, project: project, task: task}, nil
	}
	if repo == "" {
		return candidateLane{}, axi.Failure("validation_repo_required", "workspace candidates require --repo <member>", false)
	}
	members, err := workspaceMembers(ctx, db, project, task)
	if err != nil {
		return candidateLane{}, err
	}
	for _, member := range members {
		if member.repo.Repo == repo {
			return memberCandidateLane(project, task, member), nil
		}
	}
	return candidateLane{}, axi.Failure("validation_repo_invalid", "Member "+repo+" is not part of this Task", false)
}

func memberCandidateLane(project store.Project, task store.Task, member memberMerge) candidateLane {
	return candidateLane{repo: member.repo.Repo, owner: project, ownerTask: task, project: member.project, task: member.task, member: member.repo}
}

// changedCandidateLanes lists the lanes of every changed full-pipeline
// repository of the Task. A workspace Member without commits on its Task
// branch has nothing to deliver, as in Merge and the done Signal.
func changedCandidateLanes(ctx context.Context, db *store.DB, project store.Project, task store.Task) ([]candidateLane, error) {
	if !project.IsWorkspace() {
		if !isFullPipelineTask(task, "") {
			return nil, nil
		}
		return []candidateLane{{owner: project, ownerTask: task, project: project, task: task}}, nil
	}
	members, err := workspaceMembers(ctx, db, project, task)
	if err != nil {
		return nil, err
	}
	var lanes []candidateLane
	for _, member := range members {
		if member.repo.State == store.TaskRepoUnchanged || !isFullPipelineTask(task, member.repo.Repo) {
			continue
		}
		if member.repo.State != store.TaskRepoMerged {
			ahead, err := commitsAhead(ctx, member.repo.WorktreePath, member.repo.BaseRef, "refs/heads/"+task.Branch)
			if err != nil {
				return nil, memberFailure(member.repo.Repo, err)
			}
			if ahead == 0 {
				continue
			}
		}
		lanes = append(lanes, memberCandidateLane(project, task, member))
	}
	return lanes, nil
}

// localDelivery is true when the lane integrates by a local fast-forward of
// its target ref instead of a pushed Task branch and PR/MR.
func (l candidateLane) localDelivery() bool {
	return l.task.MergeMode == "local"
}

func (l candidateLane) targetBranch() string {
	return taskTargetBranch(l.project, l.task)
}

// targetHead reads the current tip of the frozen target where the lane
// delivers: origin for PR delivery, the Merge target ref for local delivery.
// An empty result means the target is missing.
func (l candidateLane) targetHead(ctx context.Context) (string, error) {
	if !l.localDelivery() {
		return remoteBranchHead(ctx, l.task.WorktreePath, "refs/heads/"+l.targetBranch())
	}
	ref, err := taskTargetRef(ctx, l.project, l.task)
	if err != nil {
		return "", err
	}
	return gitOutput(ctx, l.project.Root, "rev-parse", "--verify", ref+"^{commit}")
}

// remoteTaskHead reads the published Task branch. Local delivery never
// publishes it, so its expected remote head is always empty.
func (l candidateLane) remoteTaskHead(ctx context.Context) (string, error) {
	if l.localDelivery() {
		return "", nil
	}
	return remoteBranchHead(ctx, l.task.WorktreePath, "refs/heads/"+l.task.Branch)
}

// memberLabel prefixes a message with the Member of a workspace lane.
func (l candidateLane) memberLabel(message string) string {
	if l.repo == "" {
		return message
	}
	return l.repo + ": " + message
}

// repoFlag is the `--repo <member>` argument that names the lane in a command.
func (l candidateLane) repoFlag() string {
	if l.repo == "" {
		return ""
	}
	return " --repo " + l.repo
}
