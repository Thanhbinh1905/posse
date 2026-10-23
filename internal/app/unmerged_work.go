package app

import (
	"context"
	"fmt"
	"os"
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
	count, err := gitOutput(ctx, task.WorktreePath, "rev-list", "--count", task.BaseRef+"..HEAD")
	if err != nil {
		return "", err
	}
	if ahead, convErr := strconv.Atoi(count); convErr != nil || ahead != 0 {
		return fmt.Sprintf("branch has %s commits ahead of %s", count, task.BaseRef), nil
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
