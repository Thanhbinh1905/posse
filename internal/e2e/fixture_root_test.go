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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

const fixtureLockName = ".posse-e2e-fixture.lock"

var fixtureRootPathHandles sync.Map

func isTaskScratchRoot(path string) bool {
	return herdr.IsTaskScratchRootPath(path)
}

func pathWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func fixturePrefix(suffix string) string {
	prefix := os.Getenv("POSSE_E2E_TMP_PREFIX")
	if prefix == "" {
		prefix = "posse-e2e-"
	}
	return prefix + suffix
}

// shortFixtureRootPath uses a short symlink under a procfs directory alias when
// a long Task scratch path would make Herdr choose a sibling socket path.
func shortFixtureRootPath(t *testing.T, root string) string {
	t.Helper()
	defaultSocketPath := filepath.Join(root, "xdg", "herdr", "herdr.sock")
	if runtime.GOOS != "linux" || len(defaultSocketPath) < 100 {
		return root
	}
	parent, err := os.Open(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	fixtureRootPathHandles.Store(root, parent)
	aliasName := fmt.Sprintf("posse-e2e-%x", time.Now().UnixNano())
	aliasTarget := filepath.Join(filepath.Dir(root), aliasName)
	if err := os.Symlink(root, aliasTarget); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(aliasTarget); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove short E2E fixture alias %s: %v", aliasTarget, err)
		}
	})
	alias := fmt.Sprintf("/proc/%d/fd/%d/%s", os.Getpid(), parent.Fd(), aliasName)
	resolvedAlias, err := filepath.EvalSymlinks(alias)
	if err != nil {
		t.Fatalf("resolve short E2E fixture path %s: %v", alias, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || resolvedAlias != resolvedRoot {
		t.Fatalf("short E2E fixture path resolves to %s, want %s: %v", resolvedAlias, resolvedRoot, err)
	}
	shortSocketPath := filepath.Join(alias, "xdg", "herdr", "herdr.sock")
	if len(shortSocketPath) >= 100 || herdr.IsolatedTestSocketPath(alias) != shortSocketPath {
		t.Fatalf("short E2E fixture did not restore its default Herdr socket path: %s", herdr.IsolatedTestSocketPath(alias))
	}
	socketDir := herdr.IsolatedTestSocketDir(alias)
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove short-path Herdr socket directory %s: %v", socketDir, err)
		}
	})
	return alias
}

// newFixtureRoot keeps a lock for the lifetime of this test. A later test run
// can reclaim only roots bearing our marker whose owning test has exited.
func newFixtureRoot(t *testing.T, prefix string) string {
	t.Helper()
	parent := os.Getenv("TMPDIR")
	if parent == "" {
		parent = os.TempDir()
	}
	if configuredRoot := os.Getenv("POSSE_E2E_TMP_ROOT"); configuredRoot != "" {
		if !validTaskScratchRootPath(configuredRoot) {
			t.Fatalf("unsafe POSSE_E2E_TMP_ROOT: %q", configuredRoot)
		}
		if _, err := os.Lstat(configuredRoot); errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(configuredRoot, 0o700); err != nil {
				t.Fatalf("create isolated E2E scratch root %s: %v", configuredRoot, err)
			}
		} else if err != nil {
			t.Fatalf("inspect isolated E2E scratch root %s: %v", configuredRoot, err)
		}
		if !validTaskScratchRoot(configuredRoot) {
			t.Fatalf("unsafe POSSE_E2E_TMP_ROOT: %q", configuredRoot)
		}
		parent = configuredRoot
		if tmpDir := os.Getenv("TMPDIR"); tmpDir != "" {
			if absolute, err := filepath.Abs(tmpDir); err == nil {
				if resolved, err := filepath.EvalSymlinks(absolute); err == nil && pathWithinRoot(configuredRoot, resolved) {
					parent = resolved
				}
			}
		}
	} else if isTaskScratchRoot(parent) {
		if !validTaskScratchRoot(parent) {
			t.Fatalf("unsafe Task scratch TMPDIR: %q", parent)
		}
		t.Setenv("POSSE_E2E_TMP_ROOT", parent)
	}
	return newFixtureRootAt(t, parent, prefix)
}

func newFixtureRootAt(t *testing.T, parent, prefix string) string {
	t.Helper()
	// Installed-binary fixtures need a path outside /tmp, but must still
	// honor a Worker's non-/tmp scratch directory when one is supplied.
	if temporary := os.TempDir(); parent == "/var/tmp" && temporary != "/tmp" && !strings.HasPrefix(temporary, "/tmp/") {
		parent = temporary
	}
	configuredRoot := filepath.Clean(os.Getenv("POSSE_E2E_TMP_ROOT"))
	parent, err := filepath.Abs(parent)
	if err != nil {
		t.Fatalf("resolve E2E fixture parent: %v", err)
	}
	parent = filepath.Clean(parent)
	customRoot := configuredRoot != "." && validTaskScratchRoot(configuredRoot) && pathWithinRoot(configuredRoot, parent)
	parentIsTemp := parent == "/tmp" || parent == "/var/tmp" || customRoot
	if (!parentIsTemp && !customRoot) || !strings.HasPrefix(prefix, "posse-e2e-") {
		t.Fatalf("unsafe E2E fixture parent/prefix: %q, %q", parent, prefix)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatalf("resolve E2E fixture parent %s: %v", parent, err)
	}
	insideTemp := false
	for _, tempRoot := range []string{"/tmp", "/var/tmp"} {
		relative, err := filepath.Rel(tempRoot, resolvedParent)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			insideTemp = true
			break
		}
	}
	if !insideTemp && !customRoot || !strings.HasPrefix(prefix, "posse-e2e-") {
		t.Fatalf("unsafe E2E fixture parent/prefix: %q, %q", parent, prefix)
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
			stopFixtureWatchRuntimes(root)
			servers, err := fixtureHerdrServerProcesses(root)
			if err != nil {
				t.Errorf("inspect isolated Herdr servers for %s: %v", root, err)
			} else if len(servers) > 0 {
				t.Errorf("isolated Herdr servers survived fixture cleanup: %+v", servers)
				if err := stopFixtureHerdrServerProcesses(root, 3*time.Second); err != nil {
					t.Errorf("stop isolated Herdr servers for %s: %v", root, err)
				}
			}
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
		socketPath := herdr.IsolatedTestSocketPath(root)
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove isolated Herdr socket for %s: %v", root, err)
		}
		socketDir := herdr.IsolatedTestSocketDir(root)
		if socketDir != filepath.Join(root, "xdg", "herdr") {
			if err := os.RemoveAll(socketDir); err != nil {
				t.Errorf("remove isolated Herdr socket directory for %s: %v", root, err)
			}
		}
		if err := removeFixtureRoot(root); err != nil {
			t.Errorf("remove E2E fixture %s: %v", root, err)
		}
		if handle, ok := fixtureRootPathHandles.LoadAndDelete(root); ok {
			_ = handle.(*os.File).Close()
		}
		_ = lock.Close()
	})
	return root
}

func validTaskScratchRootPath(root string) bool {
	return filepath.Clean(root) == root && herdr.IsTaskScratchRootPath(root)
}

func validTaskScratchRoot(root string) bool {
	if !validTaskScratchRootPath(root) {
		return false
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(root)
	return err == nil && resolved == root
}

func TestIsolatedHerdrAcceptsDefaultTaskScratchTMPDIR(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux procfs to keep the non-/tmp fixture physically inside Task scratch")
	}
	taskScratch := os.Getenv("TMPDIR")
	if !validTaskScratchRoot(taskScratch) {
		var err error
		taskScratch, err = os.MkdirTemp("/tmp", "posse-e2e-task-scratch-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(taskScratch) })
		if err := os.Chmod(taskScratch, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tmpDir := filepath.Join(taskScratch, "posse", "scratch", "shop", "t1")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmpHandle, err := os.Open(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tmpHandle.Close() })
	configuredTMPDIR := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), tmpHandle.Fd())
	if _, err := os.Stat(configuredTMPDIR); err != nil {
		t.Fatalf("Task TMPDIR alias is unavailable: %v", err)
	}
	t.Setenv("TMPDIR", configuredTMPDIR)
	t.Setenv("POSSE_E2E_TMP_ROOT", taskScratch)

	root := newFixtureRoot(t, herdr.TestRootName())
	resolvedTMPDir, err := filepath.EvalSymlinks(configuredTMPDIR)
	if err != nil {
		t.Fatal(err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if resolvedParent != resolvedTMPDir {
		t.Fatalf("isolated E2E fixture is not directly under the Task TMPDIR: root=%q parent=%q TMPDIR=%q", root, resolvedParent, resolvedTMPDir)
	}
	if relative, err := filepath.Rel(taskScratch, resolvedParent); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("isolated E2E fixture escaped Task scratch: root=%q parent=%q relative=%q err=%v", root, resolvedParent, relative, err)
	}
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	childTMPDIR := filepath.Join(root, "child-tmp")
	if err := os.Mkdir(childTMPDIR, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := herdr.ValidateIsolatedEnvironment(setEnv(env, "TMPDIR", childTMPDIR)); err != nil {
		t.Fatalf("child TMPDIR override invalidated the Task scratch fixture: %v", err)
	}
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	status, err := client.Status(t.Context())
	if err != nil || !status.Running {
		t.Fatalf("private Herdr server did not start under Task TMPDIR: status=%#v err=%v", status, err)
	}
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
	resolvedRoot, err := filepath.EvalSymlinks(root)
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
		if !fixtureEnvironmentBelongsToRoot(environ, root, resolvedRoot) {
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

func fixtureEnvironmentBelongsToRoot(environ []byte, root, resolvedRoot string) bool {
	procFDPrefix := fmt.Sprintf("/proc/%d/fd/", os.Getpid())
	for _, item := range strings.Split(string(environ), "\x00") {
		key, value, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		switch key {
		case "POSSE_TEST_ROOT":
			if value == root {
				return true
			}
			if strings.HasPrefix(value, procFDPrefix) {
				if resolved, err := filepath.EvalSymlinks(value); err == nil && resolved == resolvedRoot {
					return true
				}
			}
		case "XDG_CONFIG_HOME":
			if value == filepath.Join(root, "xdg") {
				return true
			}
			if strings.HasPrefix(value, procFDPrefix) {
				if resolved, err := filepath.EvalSymlinks(value); err == nil && resolved == filepath.Join(resolvedRoot, "xdg") {
					return true
				}
			}
		}
	}
	return false
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
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		if !process.IsDir() || strings.Trim(process.Name(), "0123456789") != "" {
			continue
		}
		processPath := filepath.Join("/proc", process.Name())
		if state := e2eProcessState(processPath); state == 'Z' || state == 'X' {
			continue
		}
		environ, err := os.ReadFile(filepath.Join(processPath, "environ"))
		if err != nil { // Exited or not owned by this user.
			continue
		}
		if fixtureEnvironmentBelongsToRoot(environ, root, resolvedRoot) {
			return true, nil
		}
	}
	return false, nil
}
