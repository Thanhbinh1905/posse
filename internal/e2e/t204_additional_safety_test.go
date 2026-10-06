//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// The shim changes timing only. All Task delivery and pruning use the real CLI.
func TestPrunePreservesBranchAdvancedAfterFinalCheck(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed work\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	task := f.mustTask(t, "t1")
	merged := f.mergeOnLocalOrigin(t, task, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merged, task.GatedSHA)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	task = f.mustTask(t, "t1")
	if task.State != store.StateTornDown {
		t.Fatalf("Task did not tear down: %#v", task)
	}
	// Model a leftover local branch at its recorded, safe squash-merged head.
	ref := "refs/heads/" + task.Branch
	gitTest(t, f.env, f.repo, "update-ref", ref, task.GatedSHA)
	dry := runPosse(t, f.binary, f.repo, f.leadEnv, "remuda", "prune")
	if !strings.Contains(dry, task.Branch) {
		t.Fatalf("safe leftover branch was not planned: %s", dry)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(f.root, "branch-shim")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.root, "prune-before-branch-delete")
	resume := marker + ".continue"
	shim := "#!/bin/sh\nif [ \"$1\" = -C ] && [ \"$2\" = " + shellQuote(f.repo) + " ] && { { [ \"$3\" = branch ] && [ \"$4\" = -D ]; } || { [ \"$3\" = update-ref ] && [ \"$4\" = -d ]; }; }; then printf ready > " + shellQuote(marker) + "; while [ ! -f " + shellQuote(resume) + " ]; do sleep 0.02; done; fi\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	prune := exec.Command(f.binary, "remuda", "prune", "--yes")
	prune.Dir, prune.Env = f.repo, setEnv(f.leadEnv, "PATH", shimDir+":"+t202EnvValue(f.leadEnv, "PATH"))
	output := filepath.Join(f.root, "branch-prune-output")
	out, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	prune.Stdout, prune.Stderr = out, out
	if err := prune.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(resume, []byte("resume"), 0o600)
		_ = prune.Process.Kill()
		_ = prune.Wait()
	}()
	if !waitForCondition(10*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
		contents, _ := os.ReadFile(output)
		t.Fatalf("prune did not reach deletion after its SHA check: %s", contents)
	}
	// A normal Git writer does not participate in Posse's private Mount lock.
	writer := filepath.Join(f.root, "detached-writer")
	gitTest(t, f.env, f.repo, "worktree", "add", "--detach", writer, task.GatedSHA)
	if err := os.WriteFile(filepath.Join(writer, "unlanded-after-prune-check.txt"), []byte("unlanded work must survive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, f.env, writer, "add", "unlanded-after-prune-check.txt")
	gitTest(t, f.env, writer, "commit", "-m", "Unlanded follow-up after prune SHA check")
	advanced := strings.TrimSpace(gitTest(t, f.env, writer, "rev-parse", "HEAD"))
	gitTest(t, f.env, f.repo, "update-ref", ref, advanced, task.GatedSHA)
	gitTest(t, f.env, f.repo, "worktree", "remove", writer)
	if _, err := gitCommand(f.env, f.repo, "merge-base", "--is-ancestor", advanced, "main"); err == nil {
		t.Fatal("probe follow-up unexpectedly landed")
	}
	if err := os.WriteFile(resume, []byte("resume"), 0o600); err != nil {
		t.Fatal(err)
	}
	pruneErr := prune.Wait()
	contents, _ := os.ReadFile(output)
	if pruneErr != nil {
		t.Fatalf("prune failed instead of skipping its changed branch: %v %s", pruneErr, contents)
	}
	if !strings.Contains(string(contents), "skipped") {
		t.Errorf("prune did not report its changed branch as skipped: %s", contents)
	}
	got, refErr := gitCommand(f.env, f.repo, "rev-parse", "--verify", ref)
	if refErr != nil || strings.TrimSpace(got) != advanced {
		t.Errorf("prune did not preserve the unlanded branch tip advanced after its final check: old=%s new=%s ref=%s got=%q err=%v", task.GatedSHA, advanced, ref, got, refErr)
	}
}

func TestPruneReclaimsMappedCacheStorage(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	scratch := filepath.Join(f.home, "scratch", "shop", "t999")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(scratch, "mapped-cache")
	if err := os.WriteFile(cache, make([]byte, 65536), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(f.root, "mapped-cache-ready")
	child := startMappedCacheHolder(t, setEnv(f.env, "TMPDIR", scratch), scratch, cache, ready)
	globalTmp := filepath.Join(f.root, "global-tmp")
	globalCache := filepath.Join(globalTmp, "mapped-cache")
	if err := os.MkdirAll(globalTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalCache, make([]byte, 65536), 0o600); err != nil {
		t.Fatal(err)
	}
	globalChild := startMappedCacheHolder(t, f.env, globalTmp, globalCache, filepath.Join(f.root, "global-mapped-ready"))
	otherTaskScratch := filepath.Join(f.home, "scratch", "shop", "t998")
	if err := os.MkdirAll(otherTaskScratch, 0o700); err != nil {
		t.Fatal(err)
	}
	otherTaskID, err := f.db.CreateTask(context.Background(), f.project.ID, store.Task{Seq: 998, Type: "scout", Title: "Active other Task"})
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		from, to store.State
		source   string
	}{{store.StateSpawning, store.StateWorking, "cli"}, {store.StateWorking, store.StateDone, "worker"}, {store.StateDone, store.StateReported, "cli"}} {
		if err := f.db.Transition(context.Background(), otherTaskID, transition.from, transition.to, transition.source, "other Task fixture"); err != nil {
			t.Fatal(err)
		}
	}
	otherCache := filepath.Join(otherTaskScratch, "mapped-cache")
	if err := os.WriteFile(otherCache, make([]byte, 65536), 0o600); err != nil {
		t.Fatal(err)
	}
	otherChild := startMappedCacheHolder(t, f.env, otherTaskScratch, otherCache, filepath.Join(f.root, "other-mapped-ready"))
	if !waitForCondition(3*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }) {
		t.Fatal("mapped-cache process did not start")
	}
	mapsPath := fmt.Sprintf("/proc/%d/maps", child.Process.Pid)
	maps, err := os.ReadFile(mapsPath)
	if err != nil || !strings.Contains(string(maps), cache) {
		t.Fatalf("fixture did not retain its file mapping: %s %v", maps, err)
	}
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", child.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		target, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", child.Process.Pid, fd.Name()))
		if strings.Contains(target, cache) {
			t.Fatalf("mmap fixture unexpectedly retains an open cache descriptor: %s", target)
		}
	}
	dry := runPosse(t, f.binary, f.repo, f.leadEnv, "remuda", "prune")
	if !strings.Contains(dry, "t999") || !strings.Contains(dry, "65536") {
		t.Fatalf("mapped cache not planned: %s", dry)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "remuda", "prune", "--yes")
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch pathname remains: %v", err)
	}
	maps, err = os.ReadFile(mapsPath)
	if err == nil && strings.Contains(string(maps), cache+" (deleted)") {
		t.Errorf("successful scratch prune left a live deleted cache mapping with no open descriptor: %s", maps)
	}
	for _, foreign := range []struct {
		name  string
		child *exec.Cmd
		cache string
	}{{"global temporary directory", globalChild, globalCache}, {"another Task scratch", otherChild, otherCache}} {
		foreignMaps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", foreign.child.Process.Pid))
		if err != nil || !strings.Contains(string(foreignMaps), foreign.cache) {
			t.Errorf("prune changed mapped process in %s: cache=%s maps=%s err=%v", foreign.name, foreign.cache, foreignMaps, err)
		}
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "remuda", "prune", "--yes")
}

func startMappedCacheHolder(t *testing.T, env []string, cwd, cache, ready string) *exec.Cmd {
	t.Helper()
	command := exec.Command("python3", "-c", t204MapCachePython, cache, cwd, ready)
	command.Dir, command.Env = cwd, env
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
	})
	if !waitForCondition(3*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }) {
		t.Fatalf("mapped-cache process for %s did not start", cwd)
	}
	return command
}

const t204MapCachePython = `import ctypes, os, pathlib, sys, time
cache = pathlib.Path(sys.argv[1])
if not cache.exists():
    cache.write_bytes(bytes(65536))
libc = ctypes.CDLL(None, use_errno=True)
libc.mmap.restype = ctypes.c_void_p
libc.mmap.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_long]
fd = os.open(cache, os.O_RDONLY)
p = libc.mmap(None, 65536, 1, 2, fd, 0)
if p == ctypes.c_void_p(-1).value:
    raise OSError(ctypes.get_errno(), 'mmap failed')
os.close(fd)
os.chdir(sys.argv[2])
pathlib.Path(sys.argv[3]).write_text('ready')
while True:
    time.sleep(0.1)
`

func TestRiderMappingIsStoppedBeforeRelease(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	if err := os.WriteFile(filepath.Join(f.root, "map-cache.py"), []byte(t204MapCachePython), 0o600); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(f.root, "bin", "claude")
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	injection := "setsid python3 \"$POSSE_TEST_ROOT/map-cache.py\" \"$TMPDIR/mapped-cache\" \"$POSSE_TEST_ROOT\" \"$POSSE_TEST_ROOT/mapped-ready\" </dev/null >/dev/null 2>&1 &\n    echo $! > \"$POSSE_TEST_ROOT/mapped-pid\"\n    printf 'worker change %s\\n'"
	updated := strings.Replace(string(agent), "printf 'worker change %s\\n'", injection, 1)
	if updated == string(agent) {
		t.Fatal("Rider mapping injection did not match fixture agent")
	}
	if err := os.WriteFile(agentPath, []byte(updated), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(f.root, "ship.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed work\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	if !waitForCondition(3*time.Second, func() bool { _, err := os.Stat(filepath.Join(f.root, "mapped-ready")); return err == nil }) {
		t.Fatal("actual Rider did not start its cache mapping")
	}
	pidBytes, err := os.ReadFile(filepath.Join(f.root, "mapped-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid < 2 {
		t.Fatalf("invalid Rider child PID: %q %v", pidBytes, err)
	}
	boot, start, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		t.Fatal(err)
	}
	process := teardownFixtureProcess{PID: pid, BootID: boot, StartTime: start}
	t.Cleanup(func() {
		if teardownFixtureProcessRunning(process) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	scratch := filepath.Join(f.home, "scratch", "shop", "t1")
	cache := filepath.Join(scratch, "mapped-cache")
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil || !strings.Contains(string(maps), cache) {
		t.Fatalf("actual Rider cache mapping missing: %s %v", maps, err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	task := f.mustTask(t, "t1")
	merged := f.mergeOnLocalOrigin(t, task, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merged, task.GatedSHA)
	runPosse(t, f.binary, f.repo, f.leadEnv, "show", "t1")
	task = f.mustTask(t, "t1")
	if task.State != store.StateTornDown {
		t.Fatalf("actual Rider did not finish Teardown: %#v", task)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch path remains: %v", err)
	}
	if teardownFixtureProcessRunning(process) {
		t.Errorf("actual Rider-owned process survived Teardown despite its scratch cache mapping")
	}
}
