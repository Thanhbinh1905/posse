//go:build e2e && linux

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestGreenhouseReviewRuntimeIsolation(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	c := newGreenhouseContract(t, f)
	isolated, binary := newIsolatedGreenhouseContract(t, c)
	c.env = setEnv(c.env, "GREENHOUSE_HOME", isolated.home)

	c.change = f.start(t, "t1", "Default Greenhouse runtime", "review-default")
	head := c.authorCommit(t)
	if result := c.review(t); result["head"] != head || result["recorded"] != true {
		t.Fatalf("default review = %v; want a Verdict for %s", result, head)
	}
	sharedPID := c.daemonPID(t)
	if sharedPID <= 1 {
		t.Fatalf("invalid shared daemon PID %d", sharedPID)
	}
	if _, err := os.Stat(filepath.Join(isolated.home, "pid")); !os.IsNotExist(err) {
		t.Fatalf("default review did not ignore ambient GREENHOUSE_HOME: %v", err)
	}
	sharedState := readGreenhouseRuntimeFile(t, filepath.Join(c.home, "state.json"))
	t.Logf("default review used shared PID %d, ignoring ambient GREENHOUSE_HOME", sharedPID)

	head = c.authorCommit(t)
	args := c.reviewArgs()
	override := append(slices.Clone(args), isolated.runtimeFlags...)
	workerEnv := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", c.change.PaneID)
	if output, err := c.posse(workerEnv, c.change.WorktreePath, override...); err == nil || !strings.Contains(output, `"code":"worker_forbidden"`) {
		t.Fatalf("Worker runtime override = %v\n%s; want worker_forbidden", err, output)
	}
	alias := filepath.Join(f.root, "shared-alias")
	if err := os.Symlink(c.home, alias); err != nil {
		t.Fatal(err)
	}
	if output, err := c.posse(c.env, f.repo, append(slices.Clone(override[:len(override)-1]), alias)...); err == nil || !strings.Contains(output, "must be separate from the shared Greenhouse home") {
		t.Fatalf("shared home alias = %v\n%s; want refusal", err, output)
	}
	if output, err := c.posse(c.env, f.repo, override[:len(override)-2]...); err == nil || !strings.Contains(output, "must be supplied together") {
		t.Fatalf("binary without home = %v\n%s; want refusal", err, output)
	}
	output, err := c.posse(c.env, f.repo, override...)
	if err != nil {
		t.Fatalf("isolated review: %v\n%s", err, output)
	}
	if result := decodeGreenhouseOutput(t, output); result["head"] != head || result["recorded"] != true {
		t.Fatalf("isolated review = %v; want a Verdict for %s", result, head)
	}
	isolatedPID := isolated.daemonPID(t)
	if isolatedPID <= 1 || isolatedPID == sharedPID {
		t.Fatalf("isolated daemon PID %d, shared PID %d", isolatedPID, sharedPID)
	}
	executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(isolatedPID), "exe"))
	if err != nil || executable != binary {
		t.Fatalf("isolated daemon executable = %q, %v; want %s", executable, err, binary)
	}
	c.hold(t)
	head = c.authorCommit(t)
	shortWait := slices.Clone(override)
	shortWait[slices.Index(shortWait, "--timeout")+1] = "5000"
	output, err = c.posse(c.env, f.repo, shortWait...)
	if err != nil {
		t.Fatalf("start pending isolated review: %v\n%s", err, output)
	}
	pending := decodeGreenhouseOutput(t, output)
	if pending["outcome"] != "running" {
		t.Fatalf("isolated review = %v; want a pending grant", pending)
	}
	c.release(t)
	if output, err := c.posse(c.env, f.repo, args...); err == nil || !strings.Contains(output, `"code":"greenhouse_runtime_conflict"`) {
		t.Fatalf("resume isolated review without flags = %v\n%s; want runtime conflict", err, output)
	}
	output, err = c.posse(c.env, f.repo, override...)
	if err != nil {
		t.Fatalf("resume isolated review with the original flags: %v\n%s", err, output)
	}
	if result := decodeGreenhouseOutput(t, output); result["head"] != head || result["recorded"] != true || result["grant"] != pending["grant"] {
		t.Fatalf("resumed isolated review = %v; want recorded grant %v for %s", result, pending["grant"], head)
	}
	mismatched := slices.Clone(override)
	mismatched[slices.Index(mismatched, "--greenhouse-binary")+1] = filepath.Join(f.root, "bin", "greenhouse")
	head = c.authorCommit(t)
	if output, err := c.posse(c.env, f.repo, mismatched...); err == nil || !strings.Contains(output, "does not run") {
		t.Fatalf("different binary in the same isolated home = %v\n%s; want refusal", err, output)
	}
	if got := isolated.daemonPID(t); got != isolatedPID {
		t.Fatalf("isolated daemon PID changed from %d to %d", isolatedPID, got)
	}
	if got := c.daemonPID(t); got != sharedPID {
		t.Fatalf("shared daemon PID changed from %d to %d", sharedPID, got)
	}
	if got := readGreenhouseRuntimeFile(t, filepath.Join(c.home, "state.json")); !bytes.Equal(got, sharedState) {
		t.Fatal("isolated review changed the shared daemon's store")
	}
	t.Logf("isolated review used PID %d and %s; shared PID and store were unchanged", isolatedPID, executable)
	if result := c.review(t); result["head"] != head || result["recorded"] != true || c.daemonPID(t) != sharedPID {
		t.Fatalf("review without flags after isolated reviews = %v; want a Verdict from the shared daemon for %s", result, head)
	}
}

func newIsolatedGreenhouseContract(t *testing.T, shared *greenhouseContract) (*greenhouseContract, string) {
	t.Helper()
	isolated := *shared
	isolated.home = filepath.Join(shared.f.root, "isolated-greenhouse")
	if err := os.Mkdir(isolated.home, 0o700); err != nil {
		t.Fatal(err)
	}
	copyGreenhouseFixtureFile(t, filepath.Join(shared.home, "backends.json"), filepath.Join(isolated.home, "backends.json"), 0o600)
	binary := filepath.Join(shared.f.root, "bin", "greenhouse-isolated")
	copyGreenhouseFixtureFile(t, filepath.Join(shared.f.root, "bin", "greenhouse"), binary, 0o700)
	isolated.runtimeFlags = []string{"--greenhouse-binary", binary, "--greenhouse-home", isolated.home}
	t.Cleanup(func() { isolated.stopDaemon(t) })
	return &isolated, binary
}

func readGreenhouseRuntimeFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func copyGreenhouseFixtureFile(t *testing.T, source, target string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(target, readGreenhouseRuntimeFile(t, source), mode); err != nil {
		t.Fatal(err)
	}
}
