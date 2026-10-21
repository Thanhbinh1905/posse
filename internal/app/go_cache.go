package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	sharedGoBuildCacheLimitBytes   int64 = 8 << 30
	sharedGoBuildCacheMaxAge             = 6 * time.Hour
	sharedGoBuildCacheTrimInterval       = 6 * time.Hour
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

func (s *Service) trimWorkerGoBuildCache(ctx context.Context, home string) (string, error) {
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
	return trimWorkerGoBuildCacheOnce(ctx, home, time.Now())
}

func trimWorkerGoBuildCacheOnce(ctx context.Context, home string, now time.Time) (string, error) {
	return trimWorkerGoBuildCacheWithProcessCheck(ctx, home, now, activeGoBuildProcesses)
}

func trimWorkerGoBuildCacheWithProcessCheck(_ context.Context, home string, now time.Time, activeProcesses func() ([]string, error)) (string, error) {
	marker := filepath.Join(home, "cache", "worker-go-build-trim.stamp")
	if info, err := os.Stat(marker); err == nil && now.Sub(info.ModTime()) < sharedGoBuildCacheTrimInterval {
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
	info, err := os.Lstat(cachePath)
	if err != nil {
		return "", fmt.Errorf("inspect worker GOCACHE %s: %w", cachePath, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("worker GOCACHE %s is not a real directory", cachePath)
	}
	before, err := pathBytes(cachePath)
	if err != nil {
		return "", fmt.Errorf("measure worker GOCACHE %s: %w", cachePath, err)
	}
	if before < sharedGoBuildCacheLimitBytes {
		if err := writeGoCacheTrimMarker(marker, now); err != nil {
			return "", err
		}
		return "", nil
	}
	active, err := activeProcesses()
	if err != nil {
		return "", fmt.Errorf("inspect Go build activity: %w", err)
	}
	if len(active) > 0 {
		return "", nil
	}
	removedBytes, removedFiles, err := removeOldGoBuildCacheFiles(cachePath, now.Add(-sharedGoBuildCacheMaxAge))
	if err != nil {
		return "", err
	}
	after, err := pathBytes(cachePath)
	if err != nil {
		return "", fmt.Errorf("remeasure worker GOCACHE %s: %w", cachePath, err)
	}
	if err := writeGoCacheTrimMarker(marker, now); err != nil {
		return "", err
	}
	if removedFiles == 0 {
		return fmt.Sprintf("Posse Worker GOCACHE remains at %d bytes; no entries older than %s were eligible for removal", after, sharedGoBuildCacheMaxAge), nil
	}
	return fmt.Sprintf("removed %d old files (%d bytes) from Posse Worker GOCACHE; usage is now %d bytes", removedFiles, removedBytes, after), nil
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

func removeOldGoBuildCacheFiles(root string, cutoff time.Time) (int64, int, error) {
	var removedBytes int64
	removedFiles := 0
	directories := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if filepath.Dir(path) == root && entry.Name() == "trim.txt" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old Go cache entry %s: %w", path, err)
		}
		removedBytes += info.Size()
		removedFiles++
		return nil
	})
	if err != nil {
		return removedBytes, removedFiles, fmt.Errorf("trim old shared Go cache entries: %w", err)
	}
	sort.Slice(directories, func(i, j int) bool { return len(directories[i]) > len(directories[j]) })
	for _, directory := range directories {
		if err := os.Remove(directory); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTEMPTY) {
			return removedBytes, removedFiles, fmt.Errorf("remove empty Go cache directory %s: %w", directory, err)
		}
	}
	return removedBytes, removedFiles, nil
}

func activeGoBuildProcesses() ([]string, error) {
	if runtime.GOOS != "linux" {
		return []string{"process visibility unavailable"}, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	active := []string{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		processPath := filepath.Join("/proc", entry.Name())
		commData, commErr := os.ReadFile(filepath.Join(processPath, "comm"))
		name := ""
		if commErr == nil {
			name = strings.TrimSpace(string(commData))
		}
		if name != "go" && name != "compile" && name != "link" && name != "asm" && name != "cgo" {
			continue
		}
		commandData, _ := os.ReadFile(filepath.Join(processPath, "cmdline"))
		command := strings.Join(strings.Fields(strings.ReplaceAll(string(commandData), "\x00", " ")), " ")
		active = append(active, fmt.Sprintf("%d (%s %s)", pid, name, command))
	}
	sort.Strings(active)
	return active, nil
}
