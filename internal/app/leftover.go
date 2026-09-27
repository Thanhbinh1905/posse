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
	return persistLeftoverSnapshot(ctx, db, project, task, member)
}

// A workspace can have uncommitted follow-up edits in local, unchanged, or
// otherwise detached members, not only in the member whose PR merged.
func snapshotUnmergedMemberWork(ctx context.Context, db *store.DB, project store.Project, task store.Task, member string) error {
	if task.WorktreePath == "" {
		return nil
	}
	status, err := gitOutput(ctx, task.WorktreePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	ref := "refs/heads/" + task.Branch
	tip, tipErr := gitOutput(ctx, project.Root, "rev-parse", "--verify", ref)
	if status == "" {
		if tipErr != nil {
			return nil
		}
		if _, err := gitOutput(ctx, project.Root, "diff", "--quiet", tip, "refs/heads/"+project.DefaultBranch); err == nil {
			return nil
		}
		name := task.Branch + "-leftover"
		leftoverRef := "refs/heads/" + name
		if previous, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", leftoverRef); err == nil {
			if previous != tip {
				return fmt.Errorf("leftover branch %s already exists with different content", name)
			}
		} else if _, err := gitOutput(ctx, project.Root, "update-ref", leftoverRef, tip, strings.Repeat("0", 40)); err != nil {
			return err
		}
		return raiseLeftoverDecision(ctx, db, project, task, member, name)
	}
	if tipErr == nil {
		head, err := gitOutput(ctx, task.WorktreePath, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if head != tip {
			return fmt.Errorf("%s has edits on a detached checkout different from its Task branch", member)
		}
	}
	return persistLeftoverSnapshot(ctx, db, project, task, member)
}

func persistLeftoverSnapshot(ctx context.Context, db *store.DB, project store.Project, task store.Task, member string) error {
	path := task.WorktreePath
	head, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	name := task.Branch + "-leftover"
	ref := "refs/heads/" + name
	// A merge-only follow-up contains no content to carry forward. Compare
	// both the merged head and the current default, including staged edits.
	if _, err := gitOutput(ctx, path, "add", "-A"); err != nil {
		return err
	}
	currentTree, err := gitOutput(ctx, path, "write-tree")
	if err != nil {
		return err
	}
	for _, base := range []string{task.LandedRef, "refs/heads/" + project.DefaultBranch} {
		if base == "" {
			continue
		}
		baseTree, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", base+"^{tree}")
		if err == nil && currentTree == baseTree {
			return nil
		}
	}
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
			return fmt.Errorf("leftover branch %s already exists with different content", name)
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
	return raiseLeftoverDecision(ctx, db, project, task, member, name)
}

func raiseLeftoverDecision(ctx context.Context, db *store.DB, project store.Project, task store.Task, member, name string) error {
	if sameLeftoverTree(ctx, project.Root, "refs/heads/"+name, "refs/heads/"+project.DefaultBranch, task.LandedRef) {
		return nil
	}
	origin := "leftover:" + name
	if member != "" {
		origin = "leftover:" + member + ":" + name
	}
	_, err := db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Kind: "leftover", Origin: origin, Question: "A Leftover from the merged pull request is saved on " + name + ". Open a new Task from it or discard it?", Options: []string{"open-task", "discard"}})
	return err
}

func sameLeftoverTree(ctx context.Context, root, snapshot string, bases ...string) bool {
	tree, err := gitOutput(ctx, root, "rev-parse", "--verify", snapshot+"^{tree}")
	if err != nil {
		return false
	}
	for _, base := range bases {
		if base == "" {
			continue
		}
		baseTree, err := gitOutput(ctx, root, "rev-parse", "--verify", base+"^{tree}")
		if err == nil && tree == baseTree {
			return true
		}
	}
	return false
}

// Obsolete stale, unanswered Decisions created before merge-only Leftovers
// were filtered. Never erase the saved branch or a User's recorded answer.
func (s *Service) obsoleteEmptyLeftovers(ctx context.Context, db *store.DB, project store.Project) error {
	decisions, err := db.Decisions(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		if decision.Kind != "leftover" || !strings.HasPrefix(decision.Origin, "leftover:") || strings.HasPrefix(decision.Origin, "leftover:unrecoverable:") {
			continue
		}
		task, err := db.TaskByID(ctx, project.ID, decision.TaskID)
		if err != nil {
			return err
		}
		root, defaultBranch, ref := project.Root, project.DefaultBranch, strings.TrimPrefix(decision.Origin, "leftover:")
		if project.IsWorkspace() {
			member, branch, ok := strings.Cut(ref, ":")
			if !ok {
				continue
			}
			target, err := s.projectTarget(ctx, db, project, member)
			if err != nil {
				return err
			}
			root, defaultBranch, ref = target.Root, target.DefaultBranch, branch
		}
		if sameLeftoverTree(ctx, root, "refs/heads/"+ref, "refs/heads/"+defaultBranch, task.LandedRef) {
			if err := db.ObsoletePendingLeftoverDecision(ctx, decision.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
