package app

import (
	"context"

	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

// Serialize Git worktree locks with Mount ownership changes across the Lead,
// Lookout and CLI processes. Otherwise reconcile can relock a checkout between
// release's git worktree unlock and its database transition to idle.
func withMountStateLock(ctx context.Context, db *store.DB, run func() error) error {
	file, err := acquireFileLock(ctx, db.Path+".mount-lock")
	if err != nil {
		return err
	}
	defer file.Close()
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return run()
}
