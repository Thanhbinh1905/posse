package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func acquireMigrationLock(ctx context.Context, databasePath string) (*os.File, error) {
	return acquireStoreFileLock(ctx, databasePath+".migrate.lock", "database migration")
}

// awaitMigrationLock waits for the migration lock for as long as its holder
// keeps advancing the lock file heartbeat, so a waiter outlasts a migration
// chain of any length but gives up on a holder that stalls.
func awaitMigrationLock(ctx context.Context, databasePath string) (*os.File, error) {
	path := databasePath + ".migrate.lock"
	lastBeat := heartbeatTime(path)
	lastProgress := time.Now()
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, migrationHeartbeatInterval)
		lock, err := acquireStoreFileLock(attemptCtx, path, "database migration")
		cancel()
		if err == nil || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return lock, err
		}
		if beat := heartbeatTime(path); !beat.Equal(lastBeat) {
			lastBeat, lastProgress = beat, time.Now()
		} else if time.Since(lastProgress) >= migrationLockStallBudget {
			return nil, fmt.Errorf("%w: database migration lock holder made no progress for %s", ErrBusy, migrationLockStallBudget)
		}
	}
}

// beatMigrationLock advances the lock file heartbeat until the returned stop
// function runs, telling waiters that this holder is still migrating.
func beatMigrationLock(databasePath string) (stop func()) {
	path := databasePath + ".migrate.lock"
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(migrationHeartbeatInterval)
		defer ticker.Stop()
		for {
			now := time.Now()
			_ = os.Chtimes(path, now, now)
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func heartbeatTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func acquireStoreFileLock(ctx context.Context, path, purpose string) (*os.File, error) {
	return acquireStoreFileLockMode(ctx, path, purpose, unix.LOCK_EX)
}

func acquireStoreFileLockShared(ctx context.Context, path, purpose string) (*os.File, error) {
	return acquireStoreFileLockMode(ctx, path, purpose, unix.LOCK_SH)
}

func acquireStoreFileLockMode(ctx context.Context, path, purpose string, mode int) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s lock: %w", purpose, err)
	}
	for {
		err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			return nil, fmt.Errorf("lock %s: %w", purpose, err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
