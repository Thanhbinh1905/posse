package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestContendedFileLockIsBoundedRetryableAndDoesNotStealOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.lock")
	owner, err := acquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = acquireFileLock(ctx, path)
	if !store.IsBusy(err) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v, want typed contention", err)
	}
	var lockWait *fileLockWaitError
	if !errors.As(err, &lockWait) {
		t.Fatalf("lock error = %v, want typed file-lock contention", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("lock wait ignored caller's bound: %s", elapsed)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("owned lock file was replaced: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	retried, err := acquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatalf("lock was not available after owner exited: %v", err)
	}
	defer retried.Close()
}

func TestCanceledFileLockWaitPreservesCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.lock")
	owner, err := acquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = acquireFileLock(ctx, path)
	if !errors.Is(err, context.Canceled) || store.IsBusy(err) {
		t.Fatalf("canceled lock wait = %v", err)
	}
}

func TestContendedFetchLockNamesFetchAndWaitsForConcurrentFetch(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	owner, err := acquireRepositoryFetchLock(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	// A normal concurrent fetch releasing within the budget lets the waiter in.
	released := time.AfterFunc(500*time.Millisecond, func() { owner.Close() })
	defer released.Stop()
	waiter, err := acquireRepositoryFetchLock(context.Background(), repo)
	if err != nil {
		t.Fatalf("fetch lock wait did not outlast a short concurrent fetch: %v", err)
	}
	defer waiter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = acquireRepositoryFetchLock(ctx, repo)
	var lockWait *fileLockWaitError
	if !errors.As(err, &lockWait) || !store.IsBusy(err) {
		t.Fatalf("fetch lock error = %v, want typed retryable contention", err)
	}
	if message := err.Error(); !strings.Contains(message, "another origin fetch is in progress") || strings.Contains(message, "database") || !strings.Contains(message, "posse-fetch.lock") {
		t.Fatalf("fetch lock message = %q", message)
	}
}

func TestFetchLockWaitBudgetCoversNormalFetch(t *testing.T) {
	if fetchLockWaitBudget < 10*time.Second {
		t.Fatalf("fetchLockWaitBudget = %s, too short for a normal concurrent fetch", fetchLockWaitBudget)
	}
	if fetchLockWaitBudget > time.Minute {
		t.Fatalf("fetchLockWaitBudget = %s, unbounded for a stopped owner", fetchLockWaitBudget)
	}
}

func TestGenericFileLockMessageStillReportsDatabaseContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.lock")
	owner, err := acquireFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	_, err = acquireFileLockWithin(context.Background(), path, 0)
	if err == nil || !strings.HasPrefix(err.Error(), "database contention; retry the operation: timed out acquiring ") {
		t.Fatalf("generic lock message = %v", err)
	}
}
