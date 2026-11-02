package app

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/thanhbinh1905/posse/internal/store"
)

func claimTaskBranchAuthor(ctx context.Context, db *store.DB, task store.Task, attempt int, previousWriterStopped bool) (int64, error) {
	project, err := db.ProjectByID(ctx, task.ProjectID)
	if err != nil {
		return 0, err
	}
	if project.IsWorkspace() {
		return 0, nil
	}
	if task.WorktreePath == "" || task.Branch == "" {
		return 0, fmt.Errorf("Task has no author branch checkout")
	}
	repository, err := gitCommonDir(ctx, task.WorktreePath)
	if err != nil {
		return 0, err
	}
	head, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil {
		return 0, err
	}
	previous, previousErr := db.BranchAuthor(ctx, repository, task.Branch)
	if previousErr != nil && previousErr != store.ErrNotFound {
		return 0, previousErr
	}
	previousHead := head
	if previousErr == nil {
		previousHead = previous.HeadSHA
		if err := requireBranchHeadAncestor(ctx, task.WorktreePath, previousHead, head); err != nil {
			return 0, err
		}
		if task.PRURL != "" {
			remoteHead, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+task.Branch)
			if err != nil {
				return 0, err
			}
			if remoteHead == "" {
				return 0, store.ErrBranchAuthorHeadChanged
			}
			if err := requireBranchHeadAncestor(ctx, task.WorktreePath, remoteHead, head); err != nil {
				return 0, err
			}
		} else if candidate, candidateErr := db.LatestValidationCandidate(ctx, task.ID, ""); candidateErr == nil {
			expectedHead := candidate.SubmittedSHA
			if candidate.PromotedSHA != "" {
				expectedHead = candidate.PromotedSHA
			}
			if err := requireBranchHeadAncestor(ctx, task.WorktreePath, expectedHead, head); err != nil {
				return 0, err
			}
		} else if candidateErr != store.ErrNotFound {
			return 0, candidateErr
		}
	}
	confirmedHead, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil {
		return 0, err
	}
	if confirmedHead != head {
		return 0, store.ErrBranchAuthorHeadChanged
	}
	claim, err := db.ClaimBranchAuthor(ctx, repository, task.Branch, task.ID, attempt, previousHead, head, previousWriterStopped)
	if err != nil {
		return 0, err
	}
	return claim.Generation, nil
}

func requireBranchHeadAncestor(ctx context.Context, worktree, ancestor, head string) error {
	if _, err := gitOutput(ctx, worktree, "merge-base", "--is-ancestor", ancestor, head); err != nil {
		return store.ErrBranchAuthorHeadChanged
	}
	return nil
}

func requireWorkerBranchAuthor(ctx context.Context, db *store.DB, task store.Task) error {
	project, err := db.ProjectByID(ctx, task.ProjectID)
	if err != nil {
		return err
	}
	if project.IsWorkspace() {
		return nil
	}
	if task.Type != "change" || task.WorktreePath == "" || task.Branch == "" {
		return nil
	}
	repository, err := gitCommonDir(ctx, task.WorktreePath)
	if err != nil {
		return err
	}
	claim, err := db.BranchAuthor(ctx, repository, task.Branch)
	if err == store.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	generation, err := strconv.ParseInt(os.Getenv("POSSE_TASK_GENERATION"), 10, 64)
	if err != nil || generation < 1 || claim.TaskID != task.ID || claim.Attempt != task.Launches {
		return store.ErrBranchAuthorStale
	}
	return db.RequireBranchAuthor(ctx, repository, task.Branch, task.ID, generation)
}

func updateWorkerBranchAuthorHead(ctx context.Context, db *store.DB, task store.Task, nextHead string) error {
	repository, err := gitCommonDir(ctx, task.WorktreePath)
	if err != nil {
		return err
	}
	claim, err := db.BranchAuthor(ctx, repository, task.Branch)
	if err == store.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	generation, err := strconv.ParseInt(os.Getenv("POSSE_TASK_GENERATION"), 10, 64)
	if err != nil || generation < 1 {
		return store.ErrBranchAuthorStale
	}
	return db.UpdateBranchAuthorHead(ctx, repository, task.Branch, task.ID, generation, claim.HeadSHA, nextHead)
}

func requireCurrentTaskBranchAuthor(ctx context.Context, db *store.DB, task store.Task) error {
	project, err := db.ProjectByID(ctx, task.ProjectID)
	if err != nil {
		return err
	}
	if project.IsWorkspace() {
		return nil
	}
	if task.Type != "change" || task.WorktreePath == "" || task.Branch == "" || task.Launches < 1 || task.State != store.StateDone && task.State != store.StateMerging && task.State != store.StateWorking && task.State != store.StateStalled {
		return nil
	}
	repository, err := gitCommonDir(ctx, task.WorktreePath)
	if err != nil {
		return err
	}
	claim, err := db.BranchAuthor(ctx, repository, task.Branch)
	if err == store.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if claim.TaskID != task.ID || claim.Attempt != task.Launches {
		return store.ErrBranchAuthorStale
	}
	return db.RequireBranchAuthor(ctx, repository, task.Branch, task.ID, claim.Generation)
}
