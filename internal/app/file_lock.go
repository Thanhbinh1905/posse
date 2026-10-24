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

type fileLockWaitError struct {
	path string
	// reason replaces the generic database-contention prefix for locks that do
	// not guard store state.
	reason string
}

func (e *fileLockWaitError) Error() string {
	prefix := e.reason
	if prefix == "" {
		prefix = store.ErrBusy.Error()
	}
	return fmt.Sprintf("%s: timed out acquiring %s", prefix, e.path)
}

func (e *fileLockWaitError) Unwrap() error { return store.ErrBusy }

// A live but stopped holder cannot release its flock. Bound acquisition even
// when the command has no deadline; never unlink or steal an owned lock file.
func acquireFileLock(ctx context.Context, path string) (*os.File, error) {
	return acquireFileLockWithin(ctx, path, fileLockWaitBudget)
}

// acquireFileLockWithin waits at most budget; a zero budget tries once.
func acquireFileLockWithin(ctx context.Context, path string, budget time.Duration) (*os.File, error) {
	return acquireFileLockReason(ctx, path, budget, "")
}

// acquireFileLockReason is acquireFileLockWithin with a timeout message that
// names the contended resource; the error still unwraps to store.ErrBusy so
// callers keep treating it as retryable.
func acquireFileLockReason(ctx context.Context, path string, budget time.Duration, reason string) (*os.File, error) {
	waitCtx, cancel := context.WithTimeout(ctx, budget)
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
			return nil, &fileLockWaitError{path: path, reason: reason}
		case <-time.After(25 * time.Millisecond):
		}
	}
}
