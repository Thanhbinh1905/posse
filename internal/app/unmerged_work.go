package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/thanhbinh1905/posse/internal/store"
)

// unmergedWork returns a reason when a Change Task holds work that a teardown
// would lose, and "" when its branch has no commits beyond its base and its
// Sandbox is clean. Anything it cannot prove empty counts as work.
func (s *Service) unmergedWork(ctx context.Context, db *store.DB, project store.Project, task store.Task) (string, error) {
	if project.IsWorkspace() || task.Type != "change" || task.PRURL != "" {
		return "Task may hold work beyond its base", nil
	}
	if task.WorktreePath == "" || task.BaseRef == "" {
		return "Task has no recorded Sandbox or base ref", nil
	}
	if _, err := os.Stat(task.WorktreePath); err != nil {
		if os.IsNotExist(err) {
			return "Task Sandbox is missing", nil
		}
		return "", err
	}
	baseRef, err := currentTaskBaseRef(ctx, project, task)
	if err != nil {
		return "", err
	}
	count, err := commitsAheadOfBase(ctx, task.WorktreePath, baseRef)
	if err != nil {
		return "", err
	}
	if ahead, convErr := strconv.Atoi(count); convErr != nil || ahead != 0 {
		return fmt.Sprintf("branch has %s commits ahead of %s", count, baseRef), nil
	}
	status, err := gitStatusOutput(ctx, task.WorktreePath, "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "Sandbox has uncommitted changes", nil
	}
	if _, err := db.LatestValidationCandidate(ctx, task.ID, ""); err == nil {
		return "Task has a private validation candidate", nil
	} else if !store.IsNotFound(err) {
		return "", err
	}
	return "", nil
}

func currentTaskBaseRef(ctx context.Context, project store.Project, task store.Task) (string, error) {
	if task.BaseRef != project.DefaultBranch {
		return task.BaseRef, nil
	}
	remoteRef := "refs/remotes/origin/" + project.DefaultBranch
	if _, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "--verify", "--quiet", remoteRef+"^{commit}"); err != nil {
		if gitRefMissing(err) {
			return task.BaseRef, nil
		}
		return "", err
	}
	if _, err := gitOutput(ctx, task.WorktreePath, "merge-base", "--is-ancestor", task.BaseRef, remoteRef); err == nil {
		return remoteRef, nil
	} else {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return task.BaseRef, nil
		}
		return "", err
	}
}

// failedStartEmpty reports whether a failed Task never reached working and its
// held Sandbox proves empty, so Teardown can release it without a discard.
func (s *Service) failedStartEmpty(ctx context.Context, db *store.DB, project store.Project, task store.Task) (bool, error) {
	neverStarted, err := taskNeverStarted(ctx, db, task)
	if err != nil || !neverStarted {
		return false, err
	}
	sandbox, err := db.SandboxByTask(ctx, task.ID)
	if err != nil || sandbox.State != "held" || sandbox.TaskID != task.ID {
		return false, nil
	}
	work, err := s.unmergedWork(ctx, db, project, task)
	return work == "", err
}
