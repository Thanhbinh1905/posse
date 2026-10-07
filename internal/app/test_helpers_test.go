package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestMain(m *testing.M) {
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "HERDR_") {
			_ = os.Unsetenv(key)
		}
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// A Service without an explicit Home must never reach the real posse home,
	// and a Worker running these tests must not carry its own Worker marker in.
	_ = os.Unsetenv(workerHomeEnv)
	tmpRoot := os.TempDir()
	reapAbandonedAppTestRuns(tmpRoot)
	runRoot, err := os.MkdirTemp(tmpRoot, "posse-app-test-run-")
	if err != nil {
		panic(err)
	}
	var lock *os.File
	cleanupSetupFailure := func(cause error) {
		if lock != nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		}
		_ = removeAppTestRun(runRoot)
		panic(cause)
	}
	lock, err = os.OpenFile(filepath.Join(runRoot, ".lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		cleanupSetupFailure(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		cleanupSetupFailure(err)
	}
	home := filepath.Join(runRoot, "posse")
	codexHome := filepath.Join(runRoot, "codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		cleanupSetupFailure(err)
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		cleanupSetupFailure(err)
	}
	_ = os.Setenv("POSSE_HOME", home)
	_ = os.Setenv("CODEX_HOME", codexHome)
	code := m.Run()
	// Crash-helper subprocesses may bypass their own TestMain cleanup. Reap
	// those abandoned per-run homes before this test process exits.
	reapAbandonedAppTestRuns(tmpRoot)
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
	if err := removeAppTestRun(runRoot); err != nil {
		fmt.Fprintln(os.Stderr, "remove app test run:", err)
		code = 1
	}
	os.Exit(code)
}

func TestTestMainReapsTimedOutRun(t *testing.T) {
	if os.Getenv("POSSE_APP_TEST_TIMEOUT_CHILD") == "1" {
		time.Sleep(5 * time.Second)
		return
	}
	root := t.TempDir()
	childEnv := func(timeout bool) []string {
		env := []string{}
		for _, entry := range os.Environ() {
			key, _, found := strings.Cut(entry, "=")
			if found && (key == "TMPDIR" || key == "POSSE_APP_TEST_TIMEOUT_CHILD") {
				continue
			}
			env = append(env, entry)
		}
		env = append(env, "TMPDIR="+root)
		if timeout {
			env = append(env, "POSSE_APP_TEST_TIMEOUT_CHILD=1")
		}
		return env
	}
	command := exec.Command(os.Args[0], "-test.run=^TestTestMainReapsTimedOutRun$", "-test.timeout=100ms")
	command.Env = childEnv(true)
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 || !strings.Contains(string(output), "panic: test timed out") {
		t.Fatalf("timed test child exit=%v output=%s", err, output)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "posse-app-test-run-") {
		t.Fatalf("timeout did not leave one reapable per-run parent: entries=%v err=%v", entries, err)
	}
	command = exec.Command(os.Args[0], "-test.run=^$")
	command.Env = childEnv(false)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("next test run could not reap timeout parent: %v output=%s", err, output)
	}
	entries, err = os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("next TestMain invocation left stale run directories: entries=%v err=%v", entries, err)
	}
}

func reapAbandonedAppTestRuns(parent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "posse-app-test-run-") {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		lockPath := filepath.Join(root, ".lock")
		info, err := os.Lstat(lockPath)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			if err := removeAppTestRun(root); err != nil {
				fmt.Fprintln(os.Stderr, "reap stale app test run:", err)
			}
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
		_ = lock.Close()
	}
}

func removeAppTestRun(root string) error {
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(root)
}

func initRepo(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "init", "-b", "main")
	gitTest(t, root, "config", "user.name", "Posse Test")
	gitTest(t, root, "config", "user.email", "posse@example.test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", "README.md")
	gitTest(t, root, "commit", "-m", "initial")
}

func gitTest(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	command.Env = []string{}
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && (key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_NOSYSTEM") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func testService(home string, adapter herdr.Adapter) *Service {
	service := New(home, adapter)
	service.herdrContext = hasHerdrEnvironment
	return service
}
