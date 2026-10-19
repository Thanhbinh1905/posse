package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

// snapshotPRLeftover runs after closing the Worker pane and before resetting
// the Sandbox. It captures committed and non-ignored uncommitted work without
// changing the Task branch, and leaves ignored build artifacts out of the tree.
func snapshotPROrphan(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	if task.PRURL == "" || task.WorktreePath == "" || task.Branch == "" {
		return nil
	}
	observation, err := db.LatestPRObservation(ctx, task.ID)
	if err != nil {
		return err
	}
	return snapshotPROrphanFromObservation(ctx, db, project, task, observation, "")
}

func snapshotPROrphanFromObservation(ctx context.Context, db *store.DB, project store.Project, task store.Task, observation store.PRObservation, member string) error {
	candidate := task
	candidate.MergedRef = observation.MergeCommit
	safe, err := safeMergedPRWorktree(ctx, db, project, candidate, observation)
	if err != nil || safe {
		return err
	}
	return persistOrphanSnapshot(ctx, db, project, task, member)
}

// A workspace can have uncommitted follow-up edits in local, unchanged, or
// otherwise detached members, not only in the member whose PR merged.
func snapshotUnmergedMemberWork(ctx context.Context, db *store.DB, project store.Project, task store.Task, member string) error {
	if task.WorktreePath == "" {
		return nil
	}
	status, err := gitStatusOutput(ctx, task.WorktreePath, "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	ref := "refs/heads/" + task.Branch
	tip, tipErr := gitOutput(ctx, project.Root, "rev-parse", "--verify", ref)
	if status == "" {
		if tipErr != nil {
			return nil
		}
		if targetRef, err := mergedTaskTargetRef(ctx, project, task); err == nil {
			if _, err := gitOutput(ctx, project.Root, "diff", "--quiet", tip, targetRef); err == nil {
				return nil
			}
		}
		name, err := orphanSnapshotBranch(ctx, project.Root, task.Branch)
		if err != nil {
			return err
		}
		orphanRef := "refs/heads/" + name
		if previous, err := gitOutput(ctx, project.Root, "rev-parse", "--verify", orphanRef); err == nil {
			if previous != tip {
				return fmt.Errorf("orphan branch %s already exists with different content", name)
			}
		} else if _, err := gitOutput(ctx, project.Root, "update-ref", orphanRef, tip, strings.Repeat("0", 40)); err != nil {
			return err
		}
		return raiseOrphanDecision(ctx, db, project, task, member, name)
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
	return persistOrphanSnapshot(ctx, db, project, task, member)
}

// A 0.14 Teardown may have written its snapshot ref before recording a
// Decision. Reuse that ref without renaming it or creating a second snapshot.
func orphanSnapshotBranch(ctx context.Context, root, branch string) (string, error) {
	legacy := branch + "-leftover"
	if _, err := gitOutput(ctx, root, "rev-parse", "--verify", "refs/heads/"+legacy); err == nil {
		return legacy, nil
	} else if !isMissingGitRef(err) {
		return "", err
	}
	return branch + "-orphan", nil
}

func persistOrphanSnapshot(ctx context.Context, db *store.DB, project store.Project, task store.Task, member string) error {
	path := task.WorktreePath
	head, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	name, err := orphanSnapshotBranch(ctx, project.Root, task.Branch)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + name
	// A merge-only follow-up contains no content to carry forward. Compare
	// both the merged head and the current target, including staged edits.
	if _, err := gitOutput(ctx, path, "add", "-A"); err != nil {
		return err
	}
	currentTree, err := gitOutput(ctx, path, "write-tree")
	if err != nil {
		return err
	}
	bases := []string{task.MergedRef}
	if targetRef, err := mergedTaskTargetRef(ctx, project, task); err == nil {
		bases = append(bases, targetRef)
	}
	for _, base := range bases {
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
			return fmt.Errorf("orphan branch %s already exists with different content", name)
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
			snapshot, err = gitOutput(ctx, path, "commit-tree", tree, "-p", head, "-m", "Snapshot Orphan from merged pull request")
			if err != nil {
				return err
			}
		}
		if _, err := gitOutput(ctx, project.Root, "update-ref", ref, snapshot, strings.Repeat("0", 40)); err != nil {
			return err
		}
	}
	return raiseOrphanDecision(ctx, db, project, task, member, name)
}

func raiseOrphanDecision(ctx context.Context, db *store.DB, project store.Project, task store.Task, member, name string) error {
	bases := []string{task.MergedRef}
	if targetRef, err := mergedTaskTargetRef(ctx, project, task); err == nil {
		bases = append(bases, targetRef)
	}
	if sameOrphanTree(ctx, project.Root, "refs/heads/"+name, bases...) {
		return nil
	}
	origin := "orphan:" + name
	if member != "" {
		origin = "orphan:" + member + ":" + name
	}
	_, err := db.RaiseDecision(ctx, store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Kind: "orphan", Origin: origin, Question: "An Orphan from the merged pull request is saved on " + name + ". Open a new Task from it or discard it?", Options: []string{"open-task", "discard"}})
	return err
}

func sameOrphanTree(ctx context.Context, root, snapshot string, bases ...string) bool {
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

// Obsolete stale, unanswered Decisions created before merge-only Orphans
// were filtered. Never erase the saved branch or a User's recorded answer.
func (s *Service) obsoleteEmptyOrphans(ctx context.Context, db *store.DB, project store.Project) error {
	decisions, err := db.Decisions(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, decision := range decisions {
		if decision.Kind != "orphan" || !strings.HasPrefix(decision.Origin, "orphan:") || strings.HasPrefix(decision.Origin, "orphan:unrecoverable:") {
			continue
		}
		task, err := db.TaskByID(ctx, project.ID, decision.TaskID)
		if err != nil {
			return err
		}
		memberTask := task
		memberProject := project
		ref := strings.TrimPrefix(decision.Origin, "orphan:")
		if project.IsWorkspace() {
			member, branch, ok := strings.Cut(ref, ":")
			if !ok {
				continue
			}
			target, err := s.projectTarget(ctx, db, project, member)
			if err != nil {
				return err
			}
			memberProject.Root, memberProject.DefaultBranch, memberProject.Kind = target.Root, target.DefaultBranch, store.ProjectKindRepo
			memberTask.TargetBranch = ""
			memberTask.MergeMode = ""
			repos, err := db.TaskRepos(ctx, task.ID)
			if err != nil {
				return err
			}
			for _, repo := range repos {
				if repo.Repo == member {
					memberTask.TargetBranch = repo.TargetBranch
					memberTask.MergeMode = repo.MergeMode
					break
				}
			}
			ref = branch
		}
		targetRef, err := mergedTaskTargetRef(ctx, memberProject, memberTask)
		if err != nil {
			continue
		}
		if sameOrphanTree(ctx, memberProject.Root, "refs/heads/"+ref, targetRef, task.MergedRef) {
			if err := db.ObsoletePendingOrphanDecision(ctx, decision.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
