package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/store"
)

// A held repository Mount stays protected across Posse and Herdr restarts.
// Missing checkouts are reported once rather than recreated over lost work.
func ensureHeldMountLocks(ctx context.Context, db *store.DB, project store.Project) error {
	return withMountStateLock(ctx, db, func() error {
		mounts, err := db.Mounts(ctx, project.ID)
		if err != nil {
			return err
		}
		for _, mount := range mounts {
			if mount.State == "releasing" && mount.TaskID != 0 {
				task, err := db.TaskByID(ctx, project.ID, mount.TaskID)
				if err != nil {
					return err
				}
				if err := finishReleasingMountLocked(ctx, db, project, mount, task); err != nil {
					return err
				}
				continue
			}
			if project.IsWorkspace() || mount.State != "held" || mount.TaskID == 0 {
				continue
			}
			task, err := db.TaskByID(ctx, project.ID, mount.TaskID)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(mount.Path, ".git")); err != nil {
				if !os.IsNotExist(err) {
					return err
				}
				found, err := db.HasNotice(ctx, project.ID, task.ID, "mount_missing")
				if err != nil {
					return err
				}
				if !found {
					_, err = db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "mount_missing", Summary: fmt.Sprintf("mount-%d disappeared while held by t%d; inspect the missing checkout before recovery", mount.Number, task.Seq), DataJSON: `{}`})
				}
				if err != nil {
					return err
				}
				continue
			}
			if err := lockMount(ctx, project.Root, mount.Path, task.Seq); err != nil {
				return err
			}
		}
		return nil
	})
}

// lockClaimedMount replaces only a stale lock left by another Posse Task.
// Ownership must already be assigned to this Task in the Mount table.
func lockClaimedMount(ctx context.Context, db *store.DB, project store.Project, mount store.Mount, task store.Task) error {
	claim, err := db.MountByTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if claim.ID != mount.ID || claim.Path != mount.Path || claim.State != "held" || claim.TaskID != task.ID {
		return store.ErrStateRace
	}
	return lockAvailableMount(ctx, db, project, mount, task)
}

// Call under withMountStateLock before claiming an idle checkout, or after
// claiming a new checkout. Git protects the checkout before it becomes held.
func lockAvailableMount(ctx context.Context, db *store.DB, project store.Project, mount store.Mount, task store.Task) error {
	current, err := mountLockReason(ctx, project.Root, mount.Path)
	if err != nil {
		return err
	}
	wanted := fmt.Sprintf("posse: held by t%d", task.Seq)
	if current != "" && current != wanted {
		old, ok := posseLockTaskSeq(current)
		if !ok {
			return fmt.Errorf("refuse to replace foreign worktree lock %q on %s", current, mount.Path)
		}
		former, lookupErr := db.Task(ctx, project.ID, fmt.Sprintf("t%d", old))
		if lookupErr != nil && lookupErr != store.ErrNotFound {
			return lookupErr
		}
		if lookupErr == nil && former.MountID == mount.ID {
			return store.ErrStateRace
		}
		if err := unlockMount(ctx, project.Root, mount.Path); err != nil {
			return err
		}
	}
	return lockMount(ctx, project.Root, mount.Path, task.Seq)
}

func posseLockTaskSeq(reason string) (int, bool) {
	const prefix = "posse: held by t"
	if !strings.HasPrefix(reason, prefix) {
		return 0, false
	}
	seq, err := strconv.Atoi(strings.TrimPrefix(reason, prefix))
	return seq, err == nil && seq > 0 && reason == fmt.Sprintf("%s%d", prefix, seq)
}
