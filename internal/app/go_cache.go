package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func locateSharedGoBuildCache(ctx context.Context) (string, error) {
	cachePath := strings.TrimSpace(os.Getenv("GOCACHE"))
	if cachePath == "" {
		if goBinary, err := exec.LookPath("go"); err == nil {
			commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if output, err := exec.CommandContext(commandCtx, goBinary, "env", "GOCACHE").Output(); err == nil {
				cachePath = strings.TrimSpace(string(output))
			}
		}
		if cachePath == "" {
			cacheDir, err := os.UserCacheDir()
			if err != nil {
				return "", fmt.Errorf("locate Go build cache: %w", err)
			}
			cachePath = filepath.Join(cacheDir, "go-build")
		}
	}
	if cachePath == "" {
		return "", fmt.Errorf("GOCACHE is empty")
	}
	absolute, err := filepath.Abs(cachePath)
	if err != nil {
		return "", fmt.Errorf("resolve GOCACHE: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve GOCACHE symlinks: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func workerGoBuildCachePath(home string) string {
	return filepath.Join(home, "cache", "worker-go-build")
}

func ensureWorkerGoBuildCache(home string) (string, error) {
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve posse home for Worker GOCACHE: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absoluteHome); err == nil {
		absoluteHome = resolved
	} else {
		return "", fmt.Errorf("resolve posse home for Worker GOCACHE: %w", err)
	}
	cachePath := workerGoBuildCachePath(absoluteHome)
	for _, directory := range []string{absoluteHome, filepath.Dir(cachePath), cachePath} {
		info, err := os.Lstat(directory)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("worker GOCACHE path component is not a real directory: %s", directory)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect worker GOCACHE path %s: %w", directory, err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("create worker GOCACHE path %s: %w", directory, err)
		}
	}
	resolved, err := filepath.EvalSymlinks(cachePath)
	if err != nil {
		return "", fmt.Errorf("resolve worker GOCACHE: %w", err)
	}
	if err := validateWorkerGoBuildCachePath(resolved, absoluteHome); err != nil {
		return "", err
	}
	return resolved, nil
}

func validateWorkerGoBuildCachePath(cachePath, home string) error {
	cachePath, err := filepath.Abs(cachePath)
	if err != nil {
		return err
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return err
	}
	if filepath.Clean(cachePath) != filepath.Clean(workerGoBuildCachePath(home)) {
		return fmt.Errorf("refuse worker GOCACHE outside its Posse-owned path %s", workerGoBuildCachePath(home))
	}
	return nil
}

// workerGoCachePolicy bounds the shared Worker GOCACHE: once it reaches
// maxSize, least-recently-used entries older than minAge are removed until it
// is below maxSize again, at most once per interval.
type workerGoCachePolicy struct {
	maxSize  int64
	minAge   time.Duration
	interval time.Duration
}

func defaultWorkerGoCachePolicy() workerGoCachePolicy {
	policy, _ := workerGoCachePolicyFromConfig(config.DefaultWorkerGoCache)
	return policy
}

func workerGoCachePolicyFromConfig(settings config.WorkerGoCache) (workerGoCachePolicy, error) {
	maxSize, err := config.ParseByteSize(settings.MaxSize)
	if err != nil {
		return workerGoCachePolicy{}, fmt.Errorf("worker_go_cache.max_size %w", err)
	}
	minAge, err := time.ParseDuration(settings.MinAge)
	if err != nil || minAge < config.MinWorkerGoCacheAge {
		return workerGoCachePolicy{}, fmt.Errorf("worker_go_cache.min_age must be a duration of at least %s", config.MinWorkerGoCacheAge)
	}
	interval, err := time.ParseDuration(settings.Interval)
	if err != nil || interval <= 0 {
		return workerGoCachePolicy{}, fmt.Errorf("worker_go_cache.interval must be a positive duration")
	}
	return workerGoCachePolicy{maxSize: maxSize, minAge: minAge, interval: interval}, nil
}

// loadWorkerGoCachePolicy reads the User config only: the cache is shared by
// every Project, so no Project may loosen or tighten its bound.
func loadWorkerGoCachePolicy(home string) (workerGoCachePolicy, error) {
	cfg, err := config.Load(home, "")
	if err != nil {
		return workerGoCachePolicy{}, err
	}
	return workerGoCachePolicyFromConfig(cfg.WorkerGoCache)
}

func (s *Service) trimWorkerGoBuildCache(ctx context.Context, home string) (string, error) {
	policy, err := loadWorkerGoCachePolicy(home)
	if err != nil {
		return "", err
	}
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		return "", err
	}
	posseHome := filepath.Dir(filepath.Dir(cachePath))
	lockPath := filepath.Join(posseHome, "cache", "worker-go-build-trim.lock")
	lock, err := acquireFileLock(ctx, lockPath)
	if err != nil {
		return "", fmt.Errorf("acquire Worker GOCACHE maintenance lock: %w", err)
	}
	defer lock.Close()
	return trimWorkerGoBuildCacheOnce(home, policy, time.Now())
}

// startWorkerGoCacheTrim runs the interval-gated trim from a Project watch so
// the bound holds while Tasks stay active and no Teardown happens.
func (s *Service) startWorkerGoCacheTrim(ctx context.Context, db *store.DB, project store.Project, home string) {
	const key = "worker-go-cache"
	trimCtx, started := s.beginMaintenanceContext(key, ctx)
	if !started {
		return
	}
	go func() {
		defer s.endMaintenance(key)
		if _, err := s.trimWorkerGoBuildCache(trimCtx, home); err != nil && !errors.Is(err, context.Canceled) {
			_ = s.recordWatchMaintenanceFailure(context.Background(), db, project, fmt.Errorf("worker GOCACHE trim: %w", err))
		}
	}()
}

type goCacheEntry struct {
	path    string
	size    int64
	modTime time.Time
}

// trimWorkerGoBuildCacheOnce is safe while Workers build. Go updates an
// entry's mtime whenever it uses an entry more than an hour old, and minAge is
// at least two hours, so entries an active build touched in the last hour are
// never removed. Go's own cache trim relies on the same mtime contract. The
// fixed bucket directories are kept because running Go processes write into
// them without recreating them.
func trimWorkerGoBuildCacheOnce(home string, policy workerGoCachePolicy, now time.Time) (string, error) {
	marker := filepath.Join(home, "cache", "worker-go-build-trim.stamp")
	if info, err := os.Stat(marker); err == nil && now.Sub(info.ModTime()) < policy.interval {
		return "", nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect shared Go cache maintenance marker: %w", err)
	}
	cachePath, err := ensureWorkerGoBuildCache(home)
	if err != nil {
		return "", err
	}
	if err := validateWorkerGoBuildCachePath(cachePath, home); err != nil {
		return "", err
	}
	entries, total, err := scanGoBuildCache(cachePath)
	if err != nil {
		return "", err
	}
	if total < policy.maxSize {
		return "", writeGoCacheTrimMarker(marker, now)
	}
	removedBytes, removedFiles, err := evictGoBuildCacheEntries(entries, total, policy.maxSize, now.Add(-policy.minAge))
	if err != nil {
		return "", err
	}
	if err := writeGoCacheTrimMarker(marker, now); err != nil {
		return "", err
	}
	after := total - removedBytes
	if after >= policy.maxSize {
		return fmt.Sprintf("removed %d files (%d bytes) from Posse Worker GOCACHE; usage remains %d bytes because newer entries were used within %s", removedFiles, removedBytes, after, policy.minAge), nil
	}
	return fmt.Sprintf("removed %d files (%d bytes) from Posse Worker GOCACHE; usage is now %d bytes", removedFiles, removedBytes, after), nil
}

// scanGoBuildCache lists regular cache files, oldest first. Go's root
// trim.txt metadata is excluded, and files that vanish mid-scan are skipped.
func scanGoBuildCache(root string) ([]goCacheEntry, int64, error) {
	var entries []goCacheEntry
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) && path != root {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if filepath.Dir(path) == root && entry.Name() == "trim.txt" {
			return nil
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		entries = append(entries, goCacheEntry{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("scan worker GOCACHE %s: %w", root, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].modTime.Before(entries[j].modTime) })
	return entries, total, nil
}

// evictGoBuildCacheEntries removes the oldest entries last used before cutoff
// until usage drops below limit. Each file is rechecked just before removal so
// an entry a build has just refreshed is kept.
func evictGoBuildCacheEntries(entries []goCacheEntry, total, limit int64, cutoff time.Time) (int64, int, error) {
	var removedBytes, vanishedBytes int64
	removedFiles := 0
	for _, entry := range entries {
		if total-removedBytes-vanishedBytes < limit || !entry.modTime.Before(cutoff) {
			break
		}
		info, err := os.Lstat(entry.path)
		if os.IsNotExist(err) {
			vanishedBytes += entry.size
			continue
		}
		if err != nil {
			return removedBytes, removedFiles, fmt.Errorf("inspect Go cache entry %s: %w", entry.path, err)
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
			return removedBytes, removedFiles, fmt.Errorf("remove old Go cache entry %s: %w", entry.path, err)
		}
		removedBytes += info.Size()
		removedFiles++
	}
	return removedBytes, removedFiles, nil
}

func writeGoCacheTrimMarker(path string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create shared Go cache maintenance directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(now.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write shared Go cache maintenance marker: %w", err)
	}
	if err := os.Chtimes(path, now, now); err != nil {
		return fmt.Errorf("timestamp shared Go cache maintenance marker: %w", err)
	}
	return nil
}
