//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var guardedFixtureRoots sync.Map

// findSpawnedPosseProcesses lists live processes started from a binary named
// posse whose environment matches. Posse starts hidden Project Watches
// detached from the command that spawned them, so only a scan finds them.
func findSpawnedPosseProcesses(matches func([]byte) bool) ([]isolatedHerdrServerProcess, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []isolatedHerdrServerProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		state, startTicks := e2eProcessStat(base)
		if startTicks == "" || state == 'Z' || state == 'X' {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(base, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if filepath.Base(args[0]) != "posse" {
			continue
		}
		environ, err := os.ReadFile(filepath.Join(base, "environ"))
		if err != nil || !matches(environ) {
			continue
		}
		cwd, _ := os.Readlink(filepath.Join(base, "cwd"))
		processes = append(processes, isolatedHerdrServerProcess{PID: pid, StartTicks: startTicks, Args: strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " ")), CWD: cwd})
	}
	return processes, nil
}

func runIDMatcher(runID string) func([]byte) bool {
	return func(environ []byte) bool { return hasEnvironmentValue(environ, "POSSE_TEST_E2E_RUN_ID", runID) }
}

func runSpawnedPosseProcesses(runID string) ([]isolatedHerdrServerProcess, error) {
	return findSpawnedPosseProcesses(runIDMatcher(runID))
}

func stopRunSpawnedPosseProcesses(runID string, timeout time.Duration) error {
	return stopProcesses("spawned posse process", func() ([]isolatedHerdrServerProcess, error) {
		return runSpawnedPosseProcesses(runID)
	}, timeout)
}

func fixtureSpawnedPosseProcesses(root string) ([]isolatedHerdrServerProcess, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = root
	}
	return findSpawnedPosseProcesses(func(environ []byte) bool {
		return fixtureEnvironmentBelongsToRoot(environ, root, resolvedRoot)
	})
}

func isHiddenWatch(process isolatedHerdrServerProcess) bool {
	args := strings.Fields(process.Args)
	return len(args) == 3 && args[1] == "watch" && args[2] == "--poll-only"
}

// guardSpawnedPosseProcesses runs when the test ends. It stops the hidden
// Project Watches posse spawned from root, which have no owner but the
// fixture, then fails the test if any other posse process is still running.
// Cleanups run last-in first-out, so a guard registered after the fixture root
// runs before the root is removed.
func guardSpawnedPosseProcesses(t *testing.T, root string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	if _, loaded := guardedFixtureRoots.LoadOrStore(root, struct{}{}); loaded {
		return
	}
	t.Cleanup(func() {
		guardedFixtureRoots.Delete(root)
		find := func(hidden bool) func() ([]isolatedHerdrServerProcess, error) {
			return func() ([]isolatedHerdrServerProcess, error) {
				all, err := fixtureSpawnedPosseProcesses(root)
				var selected []isolatedHerdrServerProcess
				for _, process := range all {
					if isHiddenWatch(process) == hidden {
						selected = append(selected, process)
					}
				}
				return selected, err
			}
		}
		if err := stopProcesses("hidden Project Watch", find(true), 3*time.Second); err != nil {
			t.Errorf("stop hidden Project Watches spawned from %s: %v", root, err)
		}
		var leaked []isolatedHerdrServerProcess
		waitForCondition(5*time.Second, func() bool {
			leaked, _ = find(false)()
			return len(leaked) == 0
		})
		if len(leaked) > 0 {
			t.Errorf("test left posse processes running: %+v", leaked)
			if err := stopProcesses("spawned posse process", find(false), 3*time.Second); err != nil {
				t.Errorf("stop leaked posse processes spawned from %s: %v", root, err)
			}
		}
	})
}

func TestSpawnedPosseWatchDoesNotOutliveKilledTest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("spawned process inspection requires Linux procfs")
	}
	runHome, err := os.MkdirTemp(os.Getenv("TMPDIR"), "t404-watch-reaper-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runHome) })
	readyFile := filepath.Join(runHome, "watch-ready")
	runID := fmt.Sprintf("t404-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := stopRunSpawnedPosseProcesses(runID, 3*time.Second); err != nil {
			t.Errorf("clean up spawned posse processes for %s: %v", runID, err)
		}
	})
	env := cleanE2EEnvironment(os.Environ())
	for key, value := range map[string]string{
		"HOME":                   filepath.Join(runHome, "home"),
		"XDG_CONFIG_HOME":        filepath.Join(runHome, "xdg"),
		"POSSE_HOME":             filepath.Join(runHome, "posse"),
		"POSSE_E2E_WATCH_HELPER": "1",
		"POSSE_E2E_READY_FILE":   readyFile,
		"POSSE_E2E_TMP_PREFIX":   "posse-e2e-t404-",
		"POSSE_TEST_E2E_RUN_ID":  runID,
	} {
		env = setEnv(env, key, value)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestSpawnedPosseWatchTerminationHelper$", "-test.timeout=2m")
	command.Env = env
	// A file, not a pipe: surviving fixture processes would otherwise hold a
	// pipe open and block Wait.
	outputFile, err := os.Create(filepath.Join(runHome, "helper.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer outputFile.Close()
	command.Stdout, command.Stderr = outputFile, outputFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	helperOutput := func() string {
		data, _ := os.ReadFile(outputFile.Name())
		return string(data)
	}
	t.Cleanup(func() { _ = os.Remove(e2eReaperReportPath(command.Process.Pid)) })
	_ = command.Wait()
	rootData, err := os.ReadFile(readyFile)
	if err != nil {
		t.Fatalf("helper never reported a running Project Watch: %v\n%s", err, helperOutput())
	}
	var leaked []isolatedHerdrServerProcess
	if !waitForCondition(10*time.Second, func() bool {
		leaked, _ = runSpawnedPosseProcesses(runID)
		return len(leaked) == 0
	}) {
		t.Fatalf("hidden Project Watch survived its killed test: %+v\nhelper output: %s", leaked, helperOutput())
	}
	if err := removeFixtureRoot(string(rootData)); err != nil {
		t.Errorf("remove killed helper fixture %s: %v", rootData, err)
	}
}

// The helper starts a hidden Project Watch, then dies without running any
// cleanup, as a timed-out or killed test binary does.
func TestSpawnedPosseWatchTerminationHelper(t *testing.T) {
	if os.Getenv("POSSE_E2E_WATCH_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	f := newPRLifecycleFixture(t)
	if !waitForCondition(30*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden Watch process not running: %v", watchPIDs(f.root))
	}
	if err := os.WriteFile(os.Getenv("POSSE_E2E_READY_FILE"), []byte(f.root), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	time.Sleep(time.Minute)
}
