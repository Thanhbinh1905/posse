package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const gitFetchAttempts = 4

func gitFetch(ctx context.Context, root string, args ...string) (string, error) {
	lock, err := acquireRepositoryFetchLock(ctx, root)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	commandArgs := append([]string{"fetch"}, args...)
	for attempt := 0; ; attempt++ {
		output, err := gitOutput(ctx, root, commandArgs...)
		if err == nil || attempt+1 == gitFetchAttempts || !isTransientGitFetchLock(err) {
			return output, err
		}
		delay := 25 * time.Millisecond * time.Duration(1<<attempt)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		}
	}
}

func acquireRepositoryFetchLock(ctx context.Context, root string) (*os.File, error) {
	commonDir, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve Git common directory for fetch lock: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir, err = filepath.Abs(commonDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Git common directory for fetch lock: %w", err)
	}
	if commonDir, err = filepath.EvalSymlinks(commonDir); err != nil {
		return nil, fmt.Errorf("resolve Git common directory for fetch lock: %w", err)
	}
	return acquireFileLock(ctx, filepath.Join(commonDir, "posse-fetch.lock"))
}

func isTransientGitFetchLock(err error) bool {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "cannot lock ref") || strings.Contains(message, "another git process seems to be running") {
		return true
	}
	return strings.Contains(message, ".lock") &&
		(strings.Contains(message, "file exists") || strings.Contains(message, "unable to create") || strings.Contains(message, "could not create"))
}
