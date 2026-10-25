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

// findSpawnedProcesses lists live processes whose environment or working
// directory matches. Posse starts hidden Project Watches, and fixtures start
// shells, detached from the command that spawned them, so only a scan finds
// them. The E2E reaper and Herdr servers are covered by their own cleanup.
func findSpawnedProcesses(matches func(environ []byte, cwd string) bool) ([]isolatedHerdrServerProcess, error) {
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
		if err != nil || len(cmdline) == 0 {
			continue // Exited, or a kernel thread.
		}
		if filepath.Base(strings.Split(string(cmdline), "\x00")[0]) == "herdr" {
			continue
		}
		environ, err := os.ReadFile(filepath.Join(base, "environ"))
		if err != nil || hasEnvironmentValue(environ, "POSSE_E2E_SERVER_REAPER", "1") {
			continue
		}
		cwd, _ := os.Readlink(filepath.Join(base, "cwd"))
		if !matches(environ, cwd) {
			continue
		}
		processes = append(processes, isolatedHerdrServerProcess{PID: pid, StartTicks: startTicks, Args: strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " ")), CWD: cwd})
	}
	return processes, nil
}

func runIDMatcher(runID string) func([]byte, string) bool {
	return func(environ []byte, _ string) bool {
		return hasEnvironmentValue(environ, "POSSE_TEST_E2E_RUN_ID", runID)
	}
}

func runSpawnedProcesses(runID string) ([]isolatedHerdrServerProcess, error) {
	return findSpawnedProcesses(runIDMatcher(runID))
}

func stopRunSpawnedProcesses(runID string, timeout time.Duration) error {
	return stopProcesses("spawned process", func() ([]isolatedHerdrServerProcess, error) {
		return runSpawnedProcesses(runID)
	}, timeout)
}

func fixtureSpawnedProcesses(root string) ([]isolatedHerdrServerProcess, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = root
	}
	return findSpawnedProcesses(func(environ []byte, cwd string) bool {
		return fixtureEnvironmentBelongsToRoot(environ, root, resolvedRoot) || pathWithinRoot(root, cwd) || pathWithinRoot(resolvedRoot, cwd)
	})
}

func isHiddenWatch(process isolatedHerdrServerProcess) bool {
	args := strings.Fields(process.Args)
	return len(args) == 3 && args[1] == "watch" && args[2] == "--poll-only"
}

// guardSpawnedProcesses runs when the test ends. It stops the hidden
// Project Watches posse spawned from root, which have no owner but the
// fixture, then fails the test if any other process of the fixture is still
// running.
// Cleanups run last-in first-out, so a guard registered after the fixture root
// runs before the root is removed.
func guardSpawnedProcesses(t *testing.T, root string) {
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
				all, err := fixtureSpawnedProcesses(root)
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
			t.Errorf("test left processes running: %+v", leaked)
			if err := stopProcesses("spawned process", find(false), 3*time.Second); err != nil {
				t.Errorf("stop leaked processes spawned from %s: %v", root, err)
			}
		}
	})
}

func TestSpawnedProcessesDoNotOutliveKilledTest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("spawned process inspection requires Linux procfs")
	}
	for _, mode := range []string{"watch", "shell"} {
		t.Run(mode, func(t *testing.T) { assertSpawnedProcessReaped(t, mode) })
	}
}

func assertSpawnedProcessReaped(t *testing.T, mode string) {
	t.Helper()
	runHome, err := os.MkdirTemp(os.Getenv("TMPDIR"), "t404-"+mode+"-reaper-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runHome) })
	readyFile := filepath.Join(runHome, "ready")
	runID := fmt.Sprintf("t404-%s-%d", mode, time.Now().UnixNano())
	t.Cleanup(func() {
		if err := stopRunSpawnedProcesses(runID, 3*time.Second); err != nil {
			t.Errorf("clean up spawned processes for %s: %v", runID, err)
		}
	})
	env := cleanE2EEnvironment(os.Environ())
	for key, value := range map[string]string{
		"HOME":                   filepath.Join(runHome, "home"),
		"XDG_CONFIG_HOME":        filepath.Join(runHome, "xdg"),
		"POSSE_HOME":             filepath.Join(runHome, "posse"),
		"POSSE_E2E_WATCH_HELPER": mode,
		"POSSE_E2E_READY_FILE":   readyFile,
		"POSSE_E2E_TMP_PREFIX":   "posse-e2e-t404-",
		"POSSE_TEST_E2E_RUN_ID":  runID,
	} {
		env = setEnv(env, key, value)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestSpawnedProcessTerminationHelper$", "-test.timeout=2m")
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
		t.Fatalf("helper never reported a running %s: %v\n%s", mode, err, helperOutput())
	}
	var leaked []isolatedHerdrServerProcess
	if !waitForCondition(10*time.Second, func() bool {
		leaked, _ = runSpawnedProcesses(runID)
		return len(leaked) == 0
	}) {
		t.Fatalf("%s survived its killed test: %+v\nhelper output: %s", mode, leaked, helperOutput())
	}
	if err := removeFixtureRoot(string(rootData)); err != nil {
		t.Errorf("remove killed helper fixture %s: %v", rootData, err)
	}
}

// The helper starts a hidden Project Watch or a detached fixture shell, then
// dies without running any cleanup, as a timed-out or killed test binary does.
func TestSpawnedProcessTerminationHelper(t *testing.T) {
	mode := os.Getenv("POSSE_E2E_WATCH_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	f := newPRLifecycleFixture(t)
	switch mode {
	case "watch":
		if !waitForCondition(30*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
			t.Fatalf("hidden Watch process not running: %v", watchPIDs(f.root))
		}
	case "shell":
		// Mirrors the background process of the Teardown fixtures.
		shell := exec.Command("sh", "-c", `while [ ! -f "$POSSE_TEST_ROOT/allow-exit" ]; do sleep 0.1; done`)
		shell.Dir, shell.Env = f.repo, f.env
		shell.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := shell.Start(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	if err := os.WriteFile(os.Getenv("POSSE_E2E_READY_FILE"), []byte(f.root), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	time.Sleep(time.Minute)
}
