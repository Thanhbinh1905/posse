package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
