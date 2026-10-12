package app

import (
	"context"

	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

// Serialize worktree locks, Mount ownership changes and Task-operation claims
// across the Lead, Lookout and CLI processes. Otherwise reconcile can relock a
// checkout during release, or prune a Task branch as an operation starts.
func withMountStateLock(ctx context.Context, db *store.DB, run func() error) error {
	file, err := acquireFileLock(ctx, db.Path+".mount-lock")
	if err != nil {
		return err
	}
	defer file.Close()
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return run()
}
