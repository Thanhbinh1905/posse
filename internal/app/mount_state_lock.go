package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

// Serialize Git worktree locks with Mount ownership changes across the Lead,
// Lookout and CLI processes. Otherwise reconcile can relock a checkout between
// release's git worktree unlock and its database transition to idle.
func withMountStateLock(ctx context.Context, db *store.DB, run func() error) error {
	file, err := os.OpenFile(db.Path+".mount-lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
			return run()
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
