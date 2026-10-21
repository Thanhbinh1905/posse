package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRemoveOldGoBuildCacheFilesKeepsRecentFilesAndMetadata(t *testing.T) {
	root, err := os.MkdirTemp(os.TempDir(), "posse-go-cache-trim-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	bucket := filepath.Join(root, "00")
	if err := os.MkdirAll(bucket, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(bucket, "old-entry")
	if err := os.WriteFile(oldPath, []byte("old cache entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	recentPath := filepath.Join(bucket, "recent-entry")
	if err := os.WriteFile(recentPath, []byte("recent cache entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(root, "trim.txt")
	if err := os.WriteFile(metadataPath, []byte("cache metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "old-entry-link")
	if err := os.Symlink(oldPath, linkPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cutoff := now.Add(-time.Hour)
	oldTime := cutoff.Add(-time.Second)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metadataPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	removedBytes, removedFiles, err := removeOldGoBuildCacheFiles(root, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removedBytes != int64(len("old cache entry")) || removedFiles != 1 {
		t.Fatalf("removed %d bytes in %d files, want one stale regular cache entry", removedBytes, removedFiles)
	}
	for _, path := range []string{recentPath, metadataPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("trim removed retained file %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("stale cache entry remains or could not be inspected: %v", err)
	}
	if info, err := os.Lstat(linkPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("trim followed or removed a cache symlink: %#v %v", info, err)
	}
}

func TestWorkerGoBuildCacheDiagnosticUsesManagedCache(t *testing.T) {
	home := t.TempDir()
	cachePath := workerGoBuildCachePath(home)
	if err := os.MkdirAll(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(cachePath, "large-entry")
	file, err := os.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(sharedGoBuildCacheLimitBytes); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	status, detail, action := workerGoBuildCacheDiagnostic(home)
	if status != "warn" || !strings.Contains(detail, cachePath) || !strings.Contains(action, "Posse trims") {
		t.Fatalf("Worker cache diagnostic = %q, %q, %q", status, detail, action)
	}
	if info, err := os.Stat(entry); err != nil || info.Size() != sharedGoBuildCacheLimitBytes {
		t.Fatalf("diagnostic removed or changed the managed cache fixture: %v %v", info, err)
	}
}

func TestWorkerGoBuildCacheTrimDoesNotWaitForSandboxStateLock(t *testing.T) {
	home := t.TempDir()
	db, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stateLock, err := acquireFileLock(context.Background(), db.Path+".mount-lock")
	if err != nil {
		t.Fatal(err)
	}
	defer stateLock.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&Service{}).trimWorkerGoBuildCache(ctx, home)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cache trim failed while the Sandbox state lock was held: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Worker GOCACHE maintenance waited on the global Sandbox state lock")
	}
}

func TestWorkerGoBuildCacheTrimEventuallyRemovesOldEntries(t *testing.T) {
	home := t.TempDir()
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		t.Fatal(err)
	}
	oldEntry := filepath.Join(cachePath, "old-build-entry")
	file, err := os.Create(oldEntry)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(sharedGoBuildCacheLimitBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	oldTime := now.Add(-sharedGoBuildCacheMaxAge - time.Second)
	if err := os.Chtimes(oldEntry, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(cachePath, "trim.txt")
	if err := os.WriteFile(metadata, []byte("Go cache metadata"), 0o600); err != nil {
		t.Fatal(err)
	}

	detail, err := trimWorkerGoBuildCacheWithProcessCheck(context.Background(), home, now, func() ([]string, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "removed 1 old files") {
		t.Fatalf("Worker cache trim did not report removing the eligible old entry: %q", detail)
	}
	if _, err := os.Lstat(oldEntry); !os.IsNotExist(err) {
		t.Fatalf("old Worker cache entry remains after maintenance: %v", err)
	}
	if _, err := os.Stat(metadata); err != nil {
		t.Fatalf("Worker cache metadata was removed: %v", err)
	}
}

func TestEnsureWorkerGoBuildCacheUsesOnePosseOwnedSharedPath(t *testing.T) {
	home := t.TempDir()
	userCache := t.TempDir()
	t.Setenv("GOCACHE", userCache)
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		t.Fatal(err)
	}
	if want := workerGoBuildCachePath(home); cachePath != want || cachePath == userCache {
		t.Fatalf("Worker GOCACHE path = %q, want shared Posse-owned path %q instead of User cache %q", cachePath, want, userCache)
	}
	if err := validateWorkerGoBuildCachePath(filepath.Join(home, "scratch", "t1", "go-build"), home); err == nil {
		t.Fatal("per-Task cache path should be refused")
	}
	if err := validateWorkerGoBuildCachePath(filepath.Join(home, "..", "user-go-build"), home); err == nil {
		t.Fatal("User cache path should be refused")
	}
}
