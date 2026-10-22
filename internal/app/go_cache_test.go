package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func writeGoCacheFixture(t *testing.T, path string, size int64, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func testWorkerGoCachePolicy(maxSize int64) workerGoCachePolicy {
	return workerGoCachePolicy{maxSize: maxSize, minAge: 2 * time.Hour, interval: time.Hour}
}

func TestWorkerGoBuildCacheTrimEvictsOldestFirstAndKeepsMetadata(t *testing.T) {
	home := t.TempDir()
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	oldest := filepath.Join(cachePath, "00", "oldest-d")
	older := filepath.Join(cachePath, "01", "older-d")
	old := filepath.Join(cachePath, "02", "old-d")
	writeGoCacheFixture(t, oldest, 400, now.Add(-72*time.Hour))
	writeGoCacheFixture(t, older, 400, now.Add(-48*time.Hour))
	writeGoCacheFixture(t, old, 400, now.Add(-24*time.Hour))
	metadata := filepath.Join(cachePath, "trim.txt")
	writeGoCacheFixture(t, metadata, 10, now.Add(-96*time.Hour))
	link := filepath.Join(cachePath, "oldest-link")
	if err := os.Symlink(oldest, link); err != nil {
		t.Fatal(err)
	}

	detail, err := trimWorkerGoBuildCacheOnce(home, testWorkerGoCachePolicy(1000), now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "removed 1 files (400 bytes)") || !strings.Contains(detail, "usage is now 800 bytes") {
		t.Fatalf("trim detail = %q, want one least-recently-used entry removed to get below the limit", detail)
	}
	if _, err := os.Lstat(oldest); !os.IsNotExist(err) {
		t.Fatalf("least-recently-used entry remains: %v", err)
	}
	for _, path := range []string{older, old, metadata} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("trim removed %s beyond the size limit or Go metadata: %v", path, err)
		}
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("trim followed or removed a cache symlink: %#v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(cachePath, "00")); err != nil || !info.IsDir() {
		t.Fatalf("trim removed a Go cache bucket directory that running builds write into: %v", err)
	}
}

func TestWorkerGoBuildCacheTrimKeepsEntriesActiveWorkersUse(t *testing.T) {
	home := t.TempDir()
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale := filepath.Join(cachePath, "00", "stale-d")
	// Go refreshes an entry's mtime when it uses it and the mtime is over an
	// hour old, so an active build's entries are at most an hour old.
	inUse := filepath.Join(cachePath, "01", "in-use-d")
	writeGoCacheFixture(t, stale, 300, now.Add(-24*time.Hour))
	writeGoCacheFixture(t, inUse, 1200, now.Add(-time.Hour))

	detail, err := trimWorkerGoBuildCacheOnce(home, testWorkerGoCachePolicy(1000), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale entry remains: %v", err)
	}
	if _, err := os.Stat(inUse); err != nil {
		t.Fatalf("trim removed an entry an active Worker used within the last hour: %v", err)
	}
	if !strings.Contains(detail, "usage remains 1200 bytes because newer entries were used within 2h0m0s") {
		t.Fatalf("trim detail = %q, want the protected recent usage reported", detail)
	}
}

func TestEvictGoBuildCacheEntriesRechecksUseBeforeRemoval(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	cutoff := now.Add(-2 * time.Hour)
	refreshed := filepath.Join(root, "00", "refreshed-d")
	writeGoCacheFixture(t, refreshed, 500, now.Add(-24*time.Hour))
	entries, total, err := scanGoBuildCache(root)
	if err != nil {
		t.Fatal(err)
	}
	// A Worker build uses the entry after the scan and refreshes its mtime.
	if err := os.Chtimes(refreshed, now, now); err != nil {
		t.Fatal(err)
	}
	vanished := goCacheEntry{path: filepath.Join(root, "00", "vanished-d"), size: 200, modTime: now.Add(-48 * time.Hour)}
	removedBytes, removedFiles, err := evictGoBuildCacheEntries(append([]goCacheEntry{vanished}, entries...), total+vanished.size, 1, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if removedBytes != 0 || removedFiles != 0 {
		t.Fatalf("removed %d bytes in %d files, want none after the entry was refreshed", removedBytes, removedFiles)
	}
	if _, err := os.Stat(refreshed); err != nil {
		t.Fatalf("eviction removed an entry refreshed after the scan: %v", err)
	}
}

func TestWorkerGoBuildCacheTrimHonorsIntervalAndLimit(t *testing.T) {
	home := t.TempDir()
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	entry := filepath.Join(cachePath, "00", "old-d")
	writeGoCacheFixture(t, entry, 500, now.Add(-24*time.Hour))
	policy := testWorkerGoCachePolicy(1000)

	if detail, err := trimWorkerGoBuildCacheOnce(home, policy, now); err != nil || detail != "" {
		t.Fatalf("trim under the limit = %q, %v; want no work", detail, err)
	}
	writeGoCacheFixture(t, filepath.Join(cachePath, "01", "older-d"), 600, now.Add(-48*time.Hour))
	if detail, err := trimWorkerGoBuildCacheOnce(home, policy, now.Add(30*time.Minute)); err != nil || detail != "" {
		t.Fatalf("trim inside the interval = %q, %v; want it skipped", detail, err)
	}
	detail, err := trimWorkerGoBuildCacheOnce(home, policy, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "usage is now 500 bytes") {
		t.Fatalf("trim after the interval = %q, want usage back under the limit", detail)
	}
}

// TestWorkerGoBuildCacheStaysBoundedUnderContinuousBuilds simulates parallel
// Workers adding cache entries every hour for two days with no Teardown. The
// watch-driven trim keeps usage at most max_size plus what builds wrote within
// min_age, instead of growing with the total written.
func TestWorkerGoBuildCacheStaysBoundedUnderContinuousBuilds(t *testing.T) {
	home := t.TempDir()
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		t.Fatal(err)
	}
	const perHour int64 = 1000
	policy := testWorkerGoCachePolicy(5 * perHour)
	start := time.Now().Add(-72 * time.Hour)
	var written, peak int64
	for hour := 0; hour < 48; hour++ {
		now := start.Add(time.Duration(hour) * time.Hour)
		for i := 0; i < 4; i++ {
			writeGoCacheFixture(t, filepath.Join(cachePath, fmt.Sprintf("%02x", hour), fmt.Sprintf("entry-%d-d", i)), perHour/4, now)
		}
		written += perHour
		if _, err := trimWorkerGoBuildCacheOnce(home, policy, now); err != nil {
			t.Fatal(err)
		}
		usage, err := pathBytes(cachePath)
		if err != nil {
			t.Fatal(err)
		}
		peak = max(peak, usage)
	}
	if bound := policy.maxSize + 2*perHour; peak > bound {
		t.Fatalf("cache peaked at %d bytes after %d bytes written, want at most %d", peak, written, bound)
	}
	t.Logf("wrote %d bytes; cache peaked at %d bytes with max_size %d", written, peak, policy.maxSize)
}

func TestWatchMaintenanceTrimsWorkerGoBuildCache(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[worker_go_cache]\nmax_size = \"1KiB\"\nmin_age = \"2h\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(cachePath, "00", "stale-d")
	writeGoCacheFixture(t, stale, 2048, time.Now().Add(-3*time.Hour))
	service := &Service{}
	service.startWorkerGoCacheTrim(context.Background(), nil, store.Project{}, home)
	service.maintenanceWG.Wait()
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("watch maintenance did not trim the Worker cache above its configured max_size: %v", err)
	}
}

func TestWorkerGoCachePolicyFromConfig(t *testing.T) {
	policy, err := workerGoCachePolicyFromConfig(config.DefaultWorkerGoCache)
	if err != nil {
		t.Fatal(err)
	}
	if policy.maxSize != 8<<30 || policy.minAge != 6*time.Hour || policy.interval != time.Hour {
		t.Fatalf("default policy = %#v", policy)
	}
	if _, err := workerGoCachePolicyFromConfig(config.WorkerGoCache{MaxSize: "8GiB", MinAge: "90m", Interval: "1h"}); err == nil {
		t.Fatal("min_age below two hours should be refused: Go refreshes in-use entries only hourly")
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
	if err := file.Truncate(defaultWorkerGoCachePolicy().maxSize); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	status, detail, action := workerGoBuildCacheDiagnostic(home, defaultWorkerGoCachePolicy())
	if status != "warn" || !strings.Contains(detail, cachePath) || !strings.Contains(action, "Posse trims") || !strings.Contains(action, "worker_go_cache.max_size") {
		t.Fatalf("Worker cache diagnostic = %q, %q, %q", status, detail, action)
	}
	if info, err := os.Stat(entry); err != nil || info.Size() != defaultWorkerGoCachePolicy().maxSize {
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
