package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/store"
)

// A held repository Mount stays protected across Posse and Herdr restarts.
// Missing checkouts are reported once rather than recreated over lost work.
func ensureHeldMountLocks(ctx context.Context, db *store.DB, project store.Project) error {
	if project.IsWorkspace() {
		return nil
	}
	tasks, err := db.LiveTasks(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.WorktreePath == "" || task.MountID == 0 {
			continue
		}
		mount, err := db.MountByTask(ctx, task.ID)
		if err != nil || mount.State != "held" || mount.TaskID != task.ID {
			continue
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
}
