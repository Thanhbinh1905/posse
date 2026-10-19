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

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type teardownFixtureProcess struct {
	PID       int
	BootID    string
	StartTime string
}

func cleanupTeardownFixtureProcesses(t *testing.T, root string, names []string, processes map[string]teardownFixtureProcess) {
	t.Helper()
	for _, name := range names {
		process, known := processes[name]
		if !known {
			// Readiness or signalling may fail before the parent records the
			// identity. Only adopt a PID still carrying this fixture's root.
			contents, err := os.ReadFile(filepath.Join(root, name+"-pid"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Errorf("read %s fixture PID during cleanup: %v", name, err)
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
			if err != nil || pid < 2 {
				t.Errorf("invalid %s fixture PID during cleanup: %q", name, contents)
				continue
			}
			environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !strings.Contains("\x00"+string(environ), "\x00POSSE_TEST_ROOT="+root+"\x00") {
				t.Errorf("cannot verify %s process %d belongs to fixture during cleanup: %v", name, pid, err)
				continue
			}
			bootID, startTime, err := store.ProcessIdentityForPID(pid)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Errorf("read %s process identity during cleanup: %v", name, err)
				continue
			}
			process = teardownFixtureProcess{PID: pid, BootID: bootID, StartTime: startTime}
		}
		bootID, startTime, err := store.ProcessIdentityForPID(process.PID)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Errorf("verify %s process identity during cleanup: %v", name, err)
			continue
		}
		if bootID != process.BootID || startTime != process.StartTime {
			continue
		}
		for _, pid := range []int{-process.PID, process.PID} {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				t.Errorf("stop %s fixture process %d: %v", name, pid, err)
			}
		}
		if !waitForCondition(5*time.Second, func() bool { return !teardownFixtureProcessRunning(process) }) {
			t.Errorf("%s fixture process survived cleanup: pid=%d", name, process.PID)
		}
	}
}

func finishTeardownOwnershipLifecycle(t *testing.T, mode, binary, repo, home, root string, env, leadEnv []string, db *store.DB, project store.Project, task store.Task, processes map[string]teardownFixtureProcess) {
	t.Helper()
	scratch := filepath.Join(home, "scratch", "shop", "t1")

	switch mode {
	case "local-ordering", "local-held-prune":
		if got := runPosse(t, binary, repo, leadEnv, "merge", "t1", "--execute", "--user-approved", "User approved the isolated local Merge"); !strings.Contains(got, "state: merged") {
			t.Fatalf("local Merge did not leave a merged Task for explicit Teardown: %s", got)
		}
		if got := gitTest(t, env, repo, "show", "main:evidence.txt"); got != "evidence\n" {
			t.Fatalf("local Merge lost delivered work: %q", got)
		}
		if mode == "local-held-prune" {
			sandbox, err := db.SandboxByTask(context.Background(), task.ID)
			if err != nil || sandbox.State != "held" {
				t.Fatalf("Merge did not retain its Sandbox: %#v %v", sandbox, err)
			}
			before := strings.TrimSpace(gitTest(t, env, task.WorktreePath, "rev-parse", "HEAD"))
			dryRun := runPosse(t, binary, repo, leadEnv, "pool", "prune")
			if strings.Contains(dryRun, task.Branch) {
				t.Fatalf("prune lists held Worker branch %s as reclaimable: %s", task.Branch, dryRun)
			}
			runPosse(t, binary, repo, leadEnv, "pool", "prune", "--yes")
			after := strings.TrimSpace(gitTest(t, env, task.WorktreePath, "rev-parse", "HEAD"))
			if after != before {
				t.Fatalf("prune changed held Worker HEAD from %s to %s", before, after)
			}
			if current, err := db.SandboxByTask(context.Background(), task.ID); err != nil || current.State != "held" {
				t.Fatalf("prune changed the held Sandbox: %#v %v", current, err)
			}
			if _, err := gitCommand(env, repo, "show-ref", "--verify", "refs/heads/"+task.Branch); err != nil {
				t.Fatalf("prune deleted the checked-out branch: %v", err)
			}
			runPosse(t, binary, repo, leadEnv, "teardown", "t1")
			return
		}
		runPosse(t, binary, repo, leadEnv, "teardown", "t1")
	case "failed-discard-ordering":
		client := herdr.NewWithEnv("herdr", env)
		if _, err := client.Call(context.Background(), "workspace.close", map[string]any{"workspace_id": task.HerdrWorkspaceID}); err != nil {
			t.Fatalf("close the exited Worker workspace before discard: %v", err)
		}
		command := exec.Command(binary, "teardown", "t1", "--discard", "--user-approved", "User approved the isolated failed Worker discard")
		command.Dir, command.Env = repo, leadEnv
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "refusing approved discard while processes remain in Task-owned paths") {
			t.Fatalf("discard did not refuse the live Task-owned process: err=%v output=%s", err, output)
		}
		if !teardownFixtureProcessRunning(processes["background"]) {
			t.Fatal("approved discard stopped the Task-owned background process")
		}
		if _, err := os.Stat(filepath.Join(root, "shutdown-marker")); !os.IsNotExist(err) {
			t.Fatalf("approved discard invoked the process shutdown handler: %v", err)
		}
		if _, err := os.Stat(scratch); err != nil {
			t.Fatalf("refused discard removed Task scratch: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "allow-background-exit"), []byte("exit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(10*time.Second, func() bool {
			_, markerErr := os.Stat(filepath.Join(root, "shutdown-marker"))
			return !teardownFixtureProcessRunning(processes["background"]) && markerErr == nil
		}) {
			t.Fatal("Task-owned background process did not exit naturally")
		}
		runPosse(t, binary, repo, leadEnv, "teardown", "t1", "--discard", "--user-approved", "User approved the isolated failed Worker discard")
		bundles, err := filepath.Glob(filepath.Join(home, "projects", "shop", "tasks", "t1", "discard", "*.bundle"))
		if err != nil || len(bundles) != 1 {
			t.Fatalf("failed discard lost approved tip bundle: %v %v", bundles, err)
		}
	case "scratch-child":
		runPosse(t, binary, repo, leadEnv, "teardown", "t1")
		if !waitForCondition(7*time.Second, func() bool { return !teardownFixtureProcessRunning(processes["background"]) }) {
			t.Fatalf("Worker-created scratch process survived Teardown: pid=%d cwd=%q", processes["background"].PID, processCWD(processes["background"].PID))
		}
		assertFixtureDescriptorClosed(t, processes["background"], "3", "open-cache")
		for _, name := range []string{"global-child", "other-task-child"} {
			process := processes[name]
			if !waitForCondition(7*time.Second, func() bool { return !teardownFixtureProcessRunning(process) }) {
				t.Errorf("Worker-created %s process survived Teardown: pid=%d cwd=%q", name, process.PID, processCWD(process.PID))
			}
			assertFixtureDescriptorClosed(t, process, "4", "foreign-cache")
			cachePath := filepath.Join(root, "global-tmp", "foreign-cache")
			if name == "other-task-child" {
				cachePath = filepath.Join(root, "posse", "scratch", "shop", "t2", "foreign-cache")
			}
			if _, err := os.Stat(cachePath); err != nil {
				t.Errorf("Teardown removed cache outside the Task scratch at %s: %v", cachePath, err)
			}
		}
	default:
		t.Fatalf("unknown lifecycle %q", mode)
	}

	current, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || current.State != store.StateTornDown {
		t.Fatalf("terminal Task=%#v err=%v", current, err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("successful %s kept or recreated Task scratch: %v", mode, err)
	}
	if mode == "local-ordering" || mode == "failed-discard-ordering" {
		if !waitForCondition(7*time.Second, func() bool { return !teardownFixtureProcessRunning(processes["background"]) }) {
			t.Fatalf("shutdown writer survived Teardown: pid=%d cwd=%q", processes["background"].PID, processCWD(processes["background"].PID))
		}
		if contents, err := os.ReadFile(filepath.Join(root, "shutdown-marker")); err != nil || string(contents) != "shutdown handler ran\n" {
			t.Fatalf("Worker shutdown handler did not run before scratch cleanup: %q %v", contents, err)
		}
	}
	if mode == "scratch-child" {
		for _, name := range []string{"report.md", "evidence.txt"} {
			contents, err := os.ReadFile(filepath.Join(home, "projects", "shop", "tasks", "t1", name))
			if err != nil || len(contents) == 0 {
				t.Errorf("saved %s lost after Teardown: %q %v", name, contents, err)
			}
		}
	}
	if got := gitTest(t, env, task.WorktreePath, "status", "--porcelain"); strings.TrimSpace(got) != "" {
		t.Errorf("Sandbox not clean: %s", got)
	}
	if _, err := gitCommand(env, repo, "show-ref", "--verify", "refs/heads/"+task.Branch); err == nil {
		t.Error("finished Task local branch remains")
	}
}

func readTeardownFixtureProcess(t *testing.T, root, name string) teardownFixtureProcess {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(root, name+"-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || pid < 2 {
		t.Fatalf("invalid %s PID %q: %v", name, contents, err)
	}
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		t.Fatalf("read %s process identity: %v", name, err)
	}
	return teardownFixtureProcess{PID: pid, BootID: bootID, StartTime: startTime}
}

func teardownFixtureProcessRunning(process teardownFixtureProcess) bool {
	bootID, startTime, err := store.ProcessIdentityForPID(process.PID)
	if err != nil || bootID != process.BootID || startTime != process.StartTime {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", process.PID))
	if err != nil {
		return false
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 || closeParen+1 >= len(stat) {
		return false
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

func processCWD(pid int) string {
	cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	return cwd
}

func assertFixtureDescriptorClosed(t *testing.T, process teardownFixtureProcess, fd, name string) {
	t.Helper()
	path := fmt.Sprintf("/proc/%d/fd/%s", process.PID, fd)
	if target, err := os.Readlink(path); err == nil && strings.Contains(target, name) {
		t.Errorf("Teardown left the Worker cache descriptor open: %s -> %s", path, target)
	}
}
