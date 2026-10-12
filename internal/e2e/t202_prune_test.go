//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Pause the real prune CLI after its Task snapshot, without changing Git results.
func TestPruneRechecksScratchAfterConcurrentRide(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	ctx := context.Background()
	brief := filepath.Join(f.root, "first.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR lifecycle change\ndone_when: committed work\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	barrierMount := filepath.Join(f.home, "remuda", "shop", "mount-999")
	gitTest(t, f.env, f.repo, "worktree", "add", "--detach", barrierMount, "main")
	if _, err := f.db.ExecContext(ctx, "INSERT INTO mounts(project_id,n,path,state) VALUES(?,999,?,'broken')", f.project.ID, barrierMount); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(f.home, "scratch", "shop", "t2")
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("next Task unexpectedly has preexisting scratch: %v", err)
	}
	agentPath := filepath.Join(f.root, "bin", "claude")
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	agent = []byte(strings.Replace(string(agent), "printf 'worker change %s\\n'", "printf 'active Rider cache\\n' > \"$TMPDIR/active-cache\"\n    printf 'worker change %s\\n'", 1))
	if err := os.WriteFile(agentPath, agent, 0o700); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(f.root, "prune-shim")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.root, "prune-loaded-tasks")
	resume := marker + ".continue"
	shim := "#!/bin/sh\nif [ \"$1\" = -C ] && [ \"$2\" = " + shellQuote(barrierMount) + " ] && [ \"$3\" = status ]; then printf ready > " + shellQuote(marker) + "; while [ ! -f " + shellQuote(resume) + " ]; do sleep 0.02; done; fi\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	prune := exec.Command(f.binary, "remuda", "prune", "--yes")
	prune.Dir, prune.Env = f.repo, setEnv(f.leadEnv, "PATH", shimDir+":"+t202EnvValue(f.leadEnv, "PATH"))
	output := filepath.Join(f.root, "prune-output")
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
		t.Fatalf("prune did not reach its post-Task-snapshot barrier: %s", contents)
	}
	secondBrief := filepath.Join(f.root, "second.md")
	if err := os.WriteFile(secondBrief, []byte("---\ntype: ship\ntitle: Concurrent Rider\ndone_when: committed work\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "ride", "--brief", secondBrief, "--name", "concurrent-rider")
	var active store.Task
	if !waitForCondition(15*time.Second, func() bool {
		active, err = f.db.Task(ctx, f.project.ID, "t2")
		_, statErr := os.Stat(filepath.Join(scratch, "active-cache"))
		return err == nil && active.State == store.StateWorking && statErr == nil
	}) {
		t.Fatalf("new Rider did not use scratch: %#v %v", active, err)
	}
	mount, err := f.db.MountByTask(ctx, active.ID)
	if err != nil || mount.State != "held" {
		t.Fatalf("active Rider's Mount: %#v %v", mount, err)
	}
	if err := os.WriteFile(resume, []byte("resume"), 0o600); err != nil {
		t.Fatal(err)
	}
	pruneErr := prune.Wait()
	contents, _ := os.ReadFile(output)
	if pruneErr != nil {
		t.Fatalf("prune failed while skipping active Task scratch: %v %s", pruneErr, contents)
	}
	if strings.Contains(string(contents), filepath.Join(f.home, "scratch", "shop", "t2")) {
		t.Errorf("prune listed active Task t2 scratch as reclaimable: %s", contents)
	}
	if cache, err := os.ReadFile(filepath.Join(scratch, "active-cache")); err != nil || string(cache) != "active Rider cache\n" {
		t.Errorf("concurrent Rider's active TMPDIR data changed: %q %v; prune=%s", cache, err, contents)
	}
	current, err := f.db.Task(ctx, f.project.ID, "t2")
	if err != nil || current.State != store.StateWorking {
		t.Errorf("unexpected active Task state %#v %v", current, err)
	}
	if got := gitTest(t, f.env, active.WorktreePath, "show", "HEAD:e2e-worker-t2.txt"); got != "worker change t2\n" {
		t.Errorf("active Rider work changed: %q", got)
	}
}

func TestPruneReclaimsOrphanOpenCacheStorage(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	scratch := filepath.Join(f.home, "scratch", "shop", "t999")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(scratch, "open-cache")
	if err := os.WriteFile(cache, make([]byte, 65536), 0o600); err != nil {
		t.Fatal(err)
	}
	child := startScratchCacheHolder(t, f.env, scratch, cache, filepath.Join(f.root, "cache-process-ready"), "3")
	globalTmp := filepath.Join(f.root, "global-tmp")
	globalCache := filepath.Join(globalTmp, "foreign-cache")
	if err := os.MkdirAll(globalTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalCache, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	globalChild := startScratchCacheHolder(t, f.env, globalTmp, globalCache, filepath.Join(f.root, "global-process-ready"), "4")
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
	otherCache := filepath.Join(otherTaskScratch, "foreign-cache")
	if err := os.WriteFile(otherCache, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	otherChild := startScratchCacheHolder(t, f.env, otherTaskScratch, otherCache, filepath.Join(f.root, "other-process-ready"), "4")
	dry := runPosse(t, f.binary, f.repo, f.leadEnv, "remuda", "prune")
	if !strings.Contains(dry, "t999") || !strings.Contains(dry, "65536") {
		t.Fatalf("orphan cache missing from prune: %s", dry)
	}
	otherTask, err := f.db.Task(context.Background(), f.project.ID, "t998")
	if err != nil || otherTask.State != store.StateReported {
		t.Fatalf("foreign process Task state before prune = %#v, %v", otherTask, err)
	}
	applyOutput := runPosse(t, f.binary, f.repo, f.leadEnv, "remuda", "prune", "--yes")
	if strings.Contains(applyOutput, "t998") {
		t.Fatalf("prune listed active other Task scratch: %s", applyOutput)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch pathname remains: %v", err)
	}
	descriptor := fmt.Sprintf("/proc/%d/fd/3", child.Process.Pid)
	if _, err := os.Stat(descriptor); err == nil {
		t.Errorf("successful prune left an orphan scratch process with its cache descriptor open: %s -> %s", descriptor, readlinkE2E(descriptor))
	}
	for _, foreign := range []struct {
		name    string
		process *exec.Cmd
		file    string
	}{{"global temporary directory", globalChild, globalCache}, {"another Task scratch", otherChild, otherCache}} {
		fd := fmt.Sprintf("/proc/%d/fd/4", foreign.process.Process.Pid)
		target, err := os.Readlink(fd)
		if err != nil || !strings.Contains(target, foreign.file) {
			identityBoot, identityStart, identityErr := store.ProcessIdentityForPID(foreign.process.Process.Pid)
			stat, statErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", foreign.process.Process.Pid))
			t.Errorf("prune changed process in %s: %s -> %s err=%v identity=%q/%q %v stat=%s (%v)", foreign.name, fd, target, err, identityBoot, identityStart, identityErr, stat, statErr)
		}
	}
	runPosse(t, f.binary, f.repo, f.leadEnv, "remuda", "prune", "--yes")
}

func startScratchCacheHolder(t *testing.T, env []string, cwd, cache, ready, fd string) *exec.Cmd {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", "exec "+fd+"<"+shellQuote(cache)+"; printf ready > "+shellQuote(ready)+"; while :; do sleep 0.1; done")
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
		t.Fatalf("scratch process for %s did not open its cache", cwd)
	}
	if target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", command.Process.Pid, fd)); err != nil || !strings.Contains(target, cache) {
		t.Fatalf("scratch process for %s did not retain its cache descriptor: %s err=%v", cwd, target, err)
	}
	return command
}

func readlinkE2E(path string) string {
	value, _ := os.Readlink(path)
	return value
}

func t202EnvValue(env []string, key string) string {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return strings.TrimPrefix(entry, key+"=")
		}
	}
	return ""
}
