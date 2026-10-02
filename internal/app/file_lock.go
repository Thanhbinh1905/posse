package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
	"golang.org/x/sys/unix"
)

const fileLockWaitBudget = 3 * time.Second

// A live but stopped holder cannot release its flock. Bound acquisition even
// when the command has no deadline; never unlink or steal an owned lock file.
func acquireFileLock(ctx context.Context, path string) (*os.File, error) {
	waitCtx, cancel := context.WithTimeout(ctx, fileLockWaitBudget)
	defer cancel()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-waitCtx.Done():
			_ = file.Close()
			if errors.Is(waitCtx.Err(), context.Canceled) {
				return nil, waitCtx.Err()
			}
			return nil, fmt.Errorf("%w: timed out acquiring %s", store.ErrBusy, path)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
