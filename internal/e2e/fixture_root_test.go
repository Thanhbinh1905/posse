//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fixtureLockName = ".posse-e2e-fixture.lock"

func fixturePrefix(suffix string) string {
	prefix := os.Getenv("POSSE_E2E_TMP_PREFIX")
	if prefix == "" {
		prefix = "posse-e2e-"
	}
	return prefix + suffix
}

// newFixtureRoot keeps a lock for the lifetime of this test. A later test run
// can reclaim only roots bearing our marker whose owning test has exited.
func newFixtureRoot(t *testing.T, prefix string) string {
	t.Helper()
	parent := os.Getenv("POSSE_E2E_TMP_ROOT")
	if parent == "" {
		parent = os.TempDir()
	}
	return newFixtureRootAt(t, parent, prefix)
}

func newFixtureRootAt(t *testing.T, parent, prefix string) string {
	t.Helper()
	configuredRoot := filepath.Clean(os.Getenv("POSSE_E2E_TMP_ROOT"))
	customRoot := configuredRoot != "." && filepath.Clean(parent) == configuredRoot && strings.HasPrefix(configuredRoot, "/tmp/posse-") && strings.HasSuffix(configuredRoot, "-scratch")
	if (parent != "/tmp" && parent != "/var/tmp" && parent != os.TempDir() && !customRoot) || !strings.HasPrefix(prefix, "posse-e2e-") {
		t.Fatalf("unsafe E2E fixture parent/prefix: %q, %q", parent, prefix)
	}
	if customRoot {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			t.Fatalf("create isolated E2E scratch root %s: %v", parent, err)
		}
	}
	if err := reclaimAbandonedFixtures(parent); err != nil {
		t.Fatalf("reclaim abandoned E2E fixtures: %v", err)
	}
	root, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(root, fixtureLockName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	}
	if err != nil {
		if lock != nil {
			_ = lock.Close()
		}
		_ = os.RemoveAll(root)
		t.Fatalf("lock E2E fixture %s: %v", root, err)
	}
	t.Cleanup(func() {
		// A Herdr plugin can still be finishing a Posse command after the
		// server stops. Wait for fixture processes to exit before removing
		// their database, or a late backup write can race RemoveAll.
		if runtime.GOOS == "linux" {
			deadline := time.Now().Add(10 * time.Second)
			for {
				active, err := fixtureProcessAlive(root)
				if err != nil {
					t.Errorf("inspect E2E fixture processes %s: %v", root, err)
					break
				}
				if !active {
					break
				}
				if time.Now().After(deadline) {
					processes, processErr := fixtureProcessDetails(root)
					t.Errorf("E2E fixture processes still running for %s: %v (inspect: %v)", root, processes, processErr)
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		if err := removeFixtureRoot(root); err != nil {
			t.Errorf("remove E2E fixture %s: %v", root, err)
		}
		_ = lock.Close()
	})
	return root
}

// Go's module cache makes directories read-only. WalkDir never follows links,
// so only directories within this fixture have their owner write bit restored.
func removeFixtureRoot(root string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().Perm()&0o700 != 0o700 {
				return os.Chmod(path, info.Mode().Perm()|0o700)
			}
			return nil
		}); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		err := os.RemoveAll(root)
		if err == nil || (!errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST)) || time.Now().After(deadline) {
			return err
		}
		// A finishing Herdr hook can recreate its SQLite state while RemoveAll
		// walks the root. Retry that narrow race, but report persistent writers.
		time.Sleep(20 * time.Millisecond)
	}
}

func reclaimAbandonedFixtures(parent string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	prefix := fixturePrefix("")
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		marker := filepath.Join(root, fixtureLockName)
		info, err := os.Lstat(marker)
		if errors.Is(err, os.ErrNotExist) {
			continue // Older or unrelated roots are not ours to reclaim.
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		lock, err := os.OpenFile(marker, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lock.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				continue // A concurrent test owns this root.
			}
			return fmt.Errorf("lock %s: %w", root, err)
		}
		active, err := fixtureProcessAlive(root)
		if err == nil && !active {
			err = removeFixtureRoot(root)
		}
		_ = lock.Close()
		if err != nil {
			return fmt.Errorf("reclaim %s: %w", root, err)
		}
	}
	return nil
}

// fixtureProcessDetails identifies remaining fixture processes without exposing their full argv.
func fixtureProcessDetails(root string) ([]string, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var details []string
	for _, process := range processes {
		if !process.IsDir() || strings.Trim(process.Name(), "0123456789") != "" {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", process.Name(), "environ"))
		if err != nil {
			continue
		}
		belongs := false
		for _, item := range strings.Split(string(environ), "\x00") {
			if item == "POSSE_TEST_ROOT="+root || item == "XDG_CONFIG_HOME="+filepath.Join(root, "xdg") {
				belongs = true
				break
			}
		}
		if !belongs {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", process.Name(), "cmdline"))
		args := strings.Fields(strings.ReplaceAll(string(cmdline), "\x00", " "))
		if len(args) > 2 {
			args = args[:2]
		}
		command := strings.Join(args, " ")
		details = append(details, process.Name()+": "+command)
	}
	return details, nil
}

// A killed test can leave its Herdr server behind. Never delete a root while
// any isolated process still advertises it as its state directory.
func fixtureProcessAlive(root string) (bool, error) {
	if runtime.GOOS != "linux" {
		return true, nil // Cannot prove an orphan is inactive without procfs.
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		if !process.IsDir() || strings.Trim(process.Name(), "0123456789") != "" {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", process.Name(), "environ"))
		if err != nil { // Exited or not owned by this user.
			continue
		}
		for _, item := range strings.Split(string(environ), "\x00") {
			if item == "POSSE_TEST_ROOT="+root || item == "XDG_CONFIG_HOME="+filepath.Join(root, "xdg") {
				return true, nil
			}
		}
	}
	return false, nil
}
