package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

// snapshotPRLeftover runs after closing the Rider pane and before resetting
// the Mount. It captures committed and non-ignored uncommitted work without
// changing the Task branch, and leaves ignored build artifacts out of the tree.
func snapshotPRLeftover(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	if task.PRURL == "" || task.WorktreePath == "" || task.Branch == "" {
		return nil
	}
	observation, err := db.LatestPRObservation(ctx, task.ID)
	if err != nil {
		return err
	}
	return snapshotPRLeftoverFromObservation(ctx, db, project, task, observation, "")
}

func snapshotPRLeftoverFromObservation(ctx context.Context, db *store.DB, project store.Project, task store.Task, observation store.PRObservation, member string) error {
	candidate := task
	candidate.LandedRef = observation.MergeCommit
	safe, err := safeMergedPRWorktree(ctx, db, project, candidate, observation)
	if err != nil || safe {
		return err
	}
	path := task.WorktreePath
	branch, err := gitOutput(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != task.Branch {
		return fmt.Errorf("cannot snapshot Leftover: Mount is not on its Task branch")
	}
	head, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err := gitOutput(ctx, path, "merge-base", "--is-ancestor", observation.HeadSHA, head); err != nil {
		return fmt.Errorf("cannot snapshot Leftover: merged head is not an ancestor of the Task branch")
	}
	name := task.Branch + "-leftover"
	ref := "refs/heads/" + name
	existing, existingErr := gitOutput(ctx, project.Root, "rev-parse", "--verify", ref)
	if existingErr == nil {
		// A retry after a crash may reuse the snapshot, but never replace another branch.
		tree, err := gitOutput(ctx, path, "rev-parse", existing+"^{tree}")
		if err != nil {
			return err
		}
		if _, err := gitOutput(ctx, path, "add", "-A"); err != nil {
			return err
		}
		currentTree, err := gitOutput(ctx, path, "write-tree")
		if err != nil {
			return err
		}
		if tree != currentTree {
			return fmt.Errorf("Leftover branch %s already exists with different content", name)
		}
	} else {
		if _, err := gitOutput(ctx, path, "add", "-A"); err != nil {
			return err
		}
		tree, err := gitOutput(ctx, path, "write-tree")
		if err != nil {
			return err
		}
		headTree, err := gitOutput(ctx, path, "rev-parse", "HEAD^{tree}")
		if err != nil {
			return err
		}
		snapshot := head
		if tree != headTree {
			snapshot, err = gitOutput(ctx, path, "commit-tree", tree, "-p", head, "-m", "Snapshot Leftover from merged pull request")
			if err != nil {
				return err
			}
		}
		if _, err := gitOutput(ctx, project.Root, "update-ref", ref, snapshot, strings.Repeat("0", 40)); err != nil {
			return err
		}
	}
	origin := "leftover:" + name
	if member != "" {
		origin = "leftover:" + member + ":" + name
	}
	_, err = db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Kind: "leftover", Origin: origin, Question: "A Leftover from the merged pull request is saved on " + name + ". Open a new Task from it or discard it?", Options: []string{"open-task", "discard"}})
	return err
}
