package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/thanhbinh1905/posse/internal/store"
)

func requireReleasedTaskBranchHead(ctx context.Context, task store.Task) error {
	if task.GatedSHA == "" {
		return store.ErrBranchAuthorHeadChanged
	}
	head, err := taskBranchAuthorHead(ctx, task.WorktreePath, task.Branch)
	if err != nil || head != task.GatedSHA {
		return store.ErrBranchAuthorHeadChanged
	}
	remoteHead, err := remoteBranchHead(ctx, task.WorktreePath, "refs/heads/"+task.Branch)
	if err != nil || remoteHead != task.GatedSHA {
		return store.ErrBranchAuthorHeadChanged
	}
	return nil
}

func claimTaskBranchAuthor(ctx context.Context, db *store.DB, task store.Task, attempt int, previousWriterStopped bool) (int64, error) {
	project, err := db.ProjectByID(ctx, task.ProjectID)
	if err != nil {
		return 0, err
	}
	if project.IsWorkspace() {
		return 0, nil
	}
	if task.WorktreePath == "" || task.Branch == "" {
		return 0, fmt.Errorf("task has no author branch checkout")
	}
	lock, err := acquireTaskBranchAuthorLock(ctx, task.WorktreePath, task.Branch)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	repository, err := gitCommonDir(ctx, task.WorktreePath)
	if err != nil {
		return 0, err
	}
	head, err := taskBranchAuthorHead(ctx, task.WorktreePath, task.Branch)
	if err != nil {
		return 0, err
	}
	previous, previousErr := db.BranchAuthor(ctx, repository, task.Branch)
	if previousErr != nil && previousErr != store.ErrNotFound {
		return 0, previousErr
	}
	if previousErr == nil && previousWriterStopped && previous.TaskID != task.ID {
		return 0, store.ErrBranchAuthorForeign
	}
	previousHead := head
	if previousErr == nil {
		previousHead = previous.HeadSHA
		if task.PRURL != "" {
			if previousHead != "" {
				if err := requireBranchHeadAncestor(ctx, task.WorktreePath, previousHead, head); err != nil {
					return 0, err
				}
			}
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
		} else {
			if previousHead != "" {
				if err := requireBranchHeadAncestor(ctx, task.WorktreePath, previousHead, head); err != nil {
					return 0, err
				}
			}
			if candidate, candidateErr := db.LatestValidationCandidate(ctx, task.ID, ""); candidateErr == nil {
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
	}
	confirmedHead, err := taskBranchAuthorHead(ctx, task.WorktreePath, task.Branch)
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

func acquireTaskBranchAuthorLock(ctx context.Context, worktree, branch string) (*os.File, error) {
	commonDir, err := repositoryCommonDir(ctx, worktree)
	if err != nil {
		return nil, fmt.Errorf("resolve branch author lock directory: %w", err)
	}
	scope := sha256.Sum256([]byte(branch))
	path := filepath.Join(commonDir, fmt.Sprintf("posse-branch-author-%x.lock", scope[:12]))
	return acquireFileLockWithin(ctx, path, fileLockWaitBudget)
}

func taskBranchAuthorHead(ctx context.Context, worktree, branch string) (string, error) {
	head, err := gitOutput(ctx, worktree, "rev-parse", "refs/heads/"+branch)
	if err == nil {
		return head, nil
	}
	current, currentErr := gitOutput(ctx, worktree, "symbolic-ref", "--quiet", "--short", "HEAD")
	_, refErr := gitOutput(ctx, worktree, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if currentErr == nil && current == branch && refErr != nil {
		return "", nil
	}
	return "", err
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

func validateWorkerBranchAuthorHeadUpdate(ctx context.Context, db *store.DB, task store.Task, requiredHead, nextHead string, authorizedRewrite bool) (string, error) {
	repository, err := gitCommonDir(ctx, task.WorktreePath)
	if err != nil {
		return "", err
	}
	claim, err := db.BranchAuthor(ctx, repository, task.Branch)
	if err == store.ErrNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	generation, err := strconv.ParseInt(os.Getenv("POSSE_TASK_GENERATION"), 10, 64)
	if err != nil || generation < 1 || claim.TaskID != task.ID || claim.Attempt != task.Launches {
		return "", store.ErrBranchAuthorStale
	}
	if err := db.RequireBranchAuthor(ctx, repository, task.Branch, task.ID, generation); err != nil {
		return "", err
	}
	if requiredHead != "" && claim.HeadSHA != requiredHead {
		return "", store.ErrBranchAuthorHeadChanged
	}
	if claim.HeadSHA != "" && claim.HeadSHA != nextHead && !authorizedRewrite {
		if err := requireBranchHeadAncestor(ctx, task.WorktreePath, claim.HeadSHA, nextHead); err != nil {
			return "", err
		}
	}
	if authorizedRewrite && requiredHead == "" {
		return "", store.ErrBranchAuthorHeadChanged
	}
	return claim.HeadSHA, nil
}

func recordWorkerBranchAuthorHead(ctx context.Context, db *store.DB, task store.Task, expectedHead, nextHead string, authorizedRewrite bool) error {
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
	if err != nil || generation < 1 || claim.TaskID != task.ID || claim.Attempt != task.Launches || claim.HeadSHA != expectedHead {
		return store.ErrBranchAuthorStale
	}
	if claim.Generation != generation {
		return store.ErrBranchAuthorStale
	}
	if claim.HeadSHA != "" && claim.HeadSHA != nextHead && !authorizedRewrite {
		if err := requireBranchHeadAncestor(ctx, task.WorktreePath, claim.HeadSHA, nextHead); err != nil {
			return err
		}
	}
	if authorizedRewrite && expectedHead == "" {
		return store.ErrBranchAuthorHeadChanged
	}
	return db.UpdateBranchAuthorHead(ctx, repository, task.Branch, task.ID, generation, expectedHead, nextHead)
}

func updateWorkerBranchAuthorHead(ctx context.Context, db *store.DB, task store.Task, nextHead string) error {
	expectedHead, err := validateWorkerBranchAuthorHeadUpdate(ctx, db, task, "", nextHead, false)
	if err != nil {
		return err
	}
	return recordWorkerBranchAuthorHead(ctx, db, task, expectedHead, nextHead, false)
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
