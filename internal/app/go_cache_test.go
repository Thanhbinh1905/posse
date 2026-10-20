package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
