//go:build e2e

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/herdr"
)

// A Go test timeout panics outside the test goroutine, so t.Cleanup and this
// TestMain cannot run. A detached reaper stops daemonized test servers when
// the test process disappears; the normal path still asserts and cleans them.
func TestMain(m *testing.M) {
	if os.Getenv("POSSE_E2E_SERVER_REAPER") == "1" {
		runE2EServerReaper()
		os.Exit(0)
	}
	if os.Getenv("POSSE_T193_BLOCK_PIDFD") == "1" {
		// This helper execs Posse in an existing fixture, creates no servers,
		// and must retain the fixture's explicit Lead caller identity.
		os.Exit(m.Run())
	}
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(key, "HERDR_") {
			_ = os.Unsetenv(key)
		}
	}
	if runtime.GOOS != "linux" {
		os.Exit(m.Run())
	}
	runID := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	if os.Getenv("POSSE_E2E_TERMINATION_HELPER") != "" || os.Getenv("POSSE_E2E_WATCH_HELPER") != "" {
		runID = os.Getenv("POSSE_TEST_E2E_RUN_ID")
	}
	_ = os.Setenv("POSSE_TEST_E2E_RUN_ID", runID)
	reaper, err := startE2EServerReaper(runID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start isolated E2E server reaper:", err)
		os.Exit(1)
	}
	code := m.Run()
	keepReaper := false
	servers, scanErr := isolatedHerdrServerProcesses(runID)
	if scanErr != nil {
		fmt.Fprintln(os.Stderr, "inspect isolated E2E servers:", scanErr)
		code = 1
		keepReaper = true
	} else if len(servers) > 0 {
		fmt.Fprintf(os.Stderr, "isolated Herdr servers survived the E2E suite: %+v\n", servers)
		if err := stopIsolatedHerdrServerProcesses(runID, 3*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "stop surviving isolated E2E servers:", err)
			keepReaper = true
		}
		code = 1
	}
	if leaked, scanErr := runSpawnedPosseProcesses(runID); scanErr != nil {
		fmt.Fprintln(os.Stderr, "inspect spawned posse processes:", scanErr)
		code = 1
		keepReaper = true
	} else if len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "posse processes spawned by the E2E suite survived it: %+v\n", leaked)
		if err := stopRunSpawnedPosseProcesses(runID, 3*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "stop surviving posse processes:", err)
			keepReaper = true
		}
		code = 1
	}
	if !keepReaper {
		_ = reaper.Process.Kill()
		_ = reaper.Wait()
	}
	os.Exit(code)
}

func startE2EServerReaper(runID string) (*exec.Cmd, error) {
	start := e2eProcessStartTicks(os.Getpid())
	if start == "" {
		return nil, fmt.Errorf("read E2E process start time")
	}
	command := execgroup.Command(os.Args[0], "-test.run=^$")
	command.Env = cleanE2EEnvironment(os.Environ())
	report := e2eReaperReportPath(os.Getpid())
	_ = os.Remove(report)
	command.Env = setEnv(command.Env, "POSSE_E2E_REAPER_REPORT", report)
	command.Env = setEnv(command.Env, "POSSE_E2E_SERVER_REAPER", "1")
	command.Env = setEnv(command.Env, "POSSE_E2E_TEST_PID", strconv.Itoa(os.Getpid()))
	command.Env = setEnv(command.Env, "POSSE_E2E_TEST_START", start)
	command.Env = setEnv(command.Env, "POSSE_TEST_E2E_RUN_ID", runID)
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	command.Stdout, command.Stderr = null, null
	if err := command.Start(); err != nil {
		_ = null.Close()
		return nil, err
	}
	_ = null.Close()
	return command, nil
}

func runE2EServerReaper() {
	pid, err := strconv.Atoi(os.Getenv("POSSE_E2E_TEST_PID"))
	if err != nil || pid <= 0 {
		return
	}
	start := os.Getenv("POSSE_E2E_TEST_START")
	// Check start time as well as PID to avoid mistaking a reused PID for the test.
	for e2eProcessIsSame(pid, start) {
		time.Sleep(25 * time.Millisecond)
	}
	runID := os.Getenv("POSSE_TEST_E2E_RUN_ID")
	// Hidden Project Watches are detached (setsid), so they outlive a killed
	// test binary just like the Herdr server does.
	err = errors.Join(stopIsolatedHerdrServerProcesses(runID, 2*time.Second), stopRunSpawnedPosseProcesses(runID, 2*time.Second))
	if err != nil {
		report := os.Getenv("POSSE_E2E_REAPER_REPORT")
		if report != "" {
			_ = os.WriteFile(report, []byte(err.Error()+"\n"), 0o600)
		}
	}
}

func e2eProcessStartTicks(pid int) string {
	_, start := e2eProcessStat(filepath.Join("/proc", strconv.Itoa(pid)))
	return start
}

func e2eProcessStat(base string) (byte, string) {
	data, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return 0, ""
	}
	closeParen := strings.LastIndex(string(data), ")")
	if closeParen < 0 {
		return 0, ""
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return 0, ""
	}
	return fields[0][0], fields[19]
}

func e2eProcessIsSame(pid int, start string) bool {
	return start != "" && e2eProcessStartTicks(pid) == start
}

func e2eReaperReportPath(pid int) string {
	return filepath.Join(os.TempDir(), "posse-e2e-server-reaper-"+strconv.Itoa(pid)+".log")
}

func TestIsolatedHerdrServerDoesNotOutliveTest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("isolated Herdr process inspection requires Linux procfs")
	}
	for _, mode := range []string{"failure", "panic", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			assertIsolatedHerdrServerReaped(t, mode)
		})
	}
}

func TestIsolatedHerdrTerminationHelper(t *testing.T) {
	mode := os.Getenv("POSSE_E2E_TERMINATION_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	root := newFixtureRoot(t, herdr.TestRootName())
	if _, err := herdr.WriteIsolatedConfig(root); err != nil {
		t.Fatal(err)
	}
	env := isolatedE2EEnv(t, root)
	env = setEnv(env, "POSSE_TEST_ROOT", root)
	client := herdr.NewWithEnv("herdr", env)
	startServer(t, client)
	if err := os.WriteFile(os.Getenv("POSSE_E2E_READY_FILE"), []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "failure":
		t.Fatal("intentional isolated Herdr fixture failure")
	case "panic":
		panic("intentional isolated Herdr fixture panic")
	case "timeout":
		time.Sleep(time.Minute)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func assertIsolatedHerdrServerReaped(t *testing.T, mode string) {
	t.Helper()
	scratch := os.Getenv("TMPDIR")
	runHome, err := os.MkdirTemp(scratch, "t285-herdr-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runHome) })
	readyFile := filepath.Join(runHome, "server-ready")
	runID := fmt.Sprintf("t285-%s-%d", mode, time.Now().UnixNano())
	t.Cleanup(func() {
		if err := stopIsolatedHerdrServerProcesses(runID, 3*time.Second); err != nil {
			t.Errorf("clean up isolated Herdr servers for %s: %v", mode, err)
		}
	})
	env := cleanE2EEnvironment(os.Environ())
	for key, value := range map[string]string{
		"HOME":                         filepath.Join(runHome, "home"),
		"XDG_CONFIG_HOME":              filepath.Join(runHome, "xdg"),
		"POSSE_HOME":                   filepath.Join(runHome, "posse"),
		"POSSE_E2E_TERMINATION_HELPER": mode,
		"POSSE_E2E_READY_FILE":         readyFile,
		"POSSE_E2E_TMP_PREFIX":         "posse-e2e-t285-",
		"POSSE_TEST_E2E_RUN_ID":        runID,
	} {
		env = setEnv(env, key, value)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestIsolatedHerdrTerminationHelper$", "-test.timeout=10s")
	command.Env = env
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reaperReport := e2eReaperReportPath(command.Process.Pid)
	t.Cleanup(func() { _ = os.Remove(reaperReport) })
	readyDeadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if !processExists(command.Process.Pid) {
			waitErr := command.Wait()
			t.Fatalf("isolated Herdr helper exited before starting its server: mode=%s err=%v output=%s", mode, waitErr, output.String())
		}
		if time.Now().After(readyDeadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("isolated Herdr helper did not start its server: mode=%s output=%s", mode, output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitErr := command.Wait()
	if mode == "timeout" {
		if !strings.Contains(output.String(), "panic: test timed out") {
			t.Fatalf("helper did not reach the Go test timeout: err=%v output=%s", waitErr, output.String())
		}
	} else if waitErr == nil {
		t.Fatalf("helper unexpectedly passed its %s path", mode)
	}
	if mode == "failure" && !strings.Contains(output.String(), "intentional isolated Herdr fixture failure") {
		t.Fatalf("helper did not exercise its failure path: %s", output.String())
	}
	if mode == "panic" && !strings.Contains(output.String(), "intentional isolated Herdr fixture panic") {
		t.Fatalf("helper did not exercise its panic path: %s", output.String())
	}
	var servers []isolatedHerdrServerProcess
	reapDeadline := time.Now().Add(5 * time.Second)
	for {
		servers, err = isolatedHerdrServerProcesses(runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(servers) == 0 || time.Now().After(reapDeadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(servers) > 0 {
		reaperError, _ := os.ReadFile(reaperReport)
		_ = stopIsolatedHerdrServerProcesses(runID, 3*time.Second)
		t.Fatalf("isolated Herdr server survived test %s: %+v; reaper=%s; helper output: %s", mode, servers, reaperError, output.String())
	}
	if reaperError, err := os.ReadFile(reaperReport); err == nil {
		t.Fatalf("isolated Herdr reaper failed after test %s: %s", mode, reaperError)
	}
	if rootData, err := os.ReadFile(readyFile); err == nil {
		root := string(rootData)
		if _, err := os.Stat(root); err == nil {
			if err := removeFixtureRoot(root); err != nil {
				t.Errorf("remove timed-out isolated Herdr fixture %s: %v", root, err)
			}
		}
	}
}

func cleanE2EEnvironment(values []string) []string {
	clean := make([]string, 0, len(values))
	for _, entry := range values {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(key, "HERDR_") {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

type isolatedHerdrServerProcess struct {
	PID        int
	StartTicks string
	Args       string
	CWD        string
	Root       string
}

func isolatedHerdrServerProcesses(runID string) ([]isolatedHerdrServerProcess, error) {
	return findIsolatedHerdrServerProcesses(func(environ []byte) bool {
		return hasEnvironmentValue(environ, "POSSE_TEST_E2E_RUN_ID", runID)
	})
}

func fixtureHerdrServerProcesses(root string) ([]isolatedHerdrServerProcess, error) {
	return findIsolatedHerdrServerProcesses(func(environ []byte) bool {
		return environmentValue(environ, "POSSE_TEST_ROOT") == root || environmentValue(environ, "XDG_CONFIG_HOME") == filepath.Join(root, "xdg")
	})
}

func findIsolatedHerdrServerProcesses(matches func([]byte) bool) ([]isolatedHerdrServerProcess, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var servers []isolatedHerdrServerProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		state, startTicks := e2eProcessStat(base)
		if startTicks == "" || state == 'Z' || state == 'X' {
			continue
		}
		environ, err := os.ReadFile(filepath.Join(base, "environ"))
		if err != nil || !hasEnvironmentValue(environ, "POSSE_TEST_HERDR", "1") || !matches(environ) {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(base, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		if len(args) < 2 || filepath.Base(args[0]) != "herdr" || args[1] != "server" {
			continue
		}
		cwd, _ := os.Readlink(filepath.Join(base, "cwd"))
		root := environmentValue(environ, "POSSE_TEST_ROOT")
		if root == "" {
			xdg := environmentValue(environ, "XDG_CONFIG_HOME")
			if filepath.Base(xdg) == "xdg" {
				root = filepath.Dir(xdg)
			}
		}
		servers = append(servers, isolatedHerdrServerProcess{PID: pid, StartTicks: startTicks, Args: strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " ")), CWD: cwd, Root: root})
	}
	return servers, nil
}

func e2eProcessState(base string) byte {
	state, _ := e2eProcessStat(base)
	return state
}

func stopIsolatedHerdrServerProcesses(runID string, timeout time.Duration) error {
	return stopMatchingIsolatedHerdrServers(func(environ []byte) bool {
		return hasEnvironmentValue(environ, "POSSE_TEST_E2E_RUN_ID", runID)
	}, timeout)
}

func stopFixtureHerdrServerProcesses(root string, timeout time.Duration) error {
	return stopMatchingIsolatedHerdrServers(func(environ []byte) bool {
		return environmentValue(environ, "POSSE_TEST_ROOT") == root || environmentValue(environ, "XDG_CONFIG_HOME") == filepath.Join(root, "xdg")
	}, timeout)
}

func stopMatchingIsolatedHerdrServers(matches func([]byte) bool, timeout time.Duration) error {
	return stopProcesses("isolated Herdr server", func() ([]isolatedHerdrServerProcess, error) {
		return findIsolatedHerdrServerProcesses(matches)
	}, timeout)
}

func stopProcesses(kind string, find func() ([]isolatedHerdrServerProcess, error), timeout time.Duration) error {
	processes, err := find()
	if err != nil {
		return err
	}
	for _, process := range processes {
		if err := signalIsolatedHerdrServer(process, syscall.SIGTERM); err != nil {
			return fmt.Errorf("send SIGTERM to %s %d: %w", kind, process.PID, err)
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		processes, err = find()
		if err != nil || len(processes) == 0 {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	for _, process := range processes {
		if err := signalIsolatedHerdrServer(process, syscall.SIGKILL); err != nil {
			return fmt.Errorf("send SIGKILL to %s %d: %w", kind, process.PID, err)
		}
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		processes, err = find()
		if err != nil || len(processes) == 0 {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("%ss remain after kill: %+v", kind, processes)
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func hasEnvironmentValue(environ []byte, key, value string) bool {
	return environmentValue(environ, key) == value
}

func environmentValue(environ []byte, key string) string {
	for _, entry := range strings.Split(string(environ), "\x00") {
		name, value, found := strings.Cut(entry, "=")
		if found && name == key {
			return value
		}
	}
	return ""
}
