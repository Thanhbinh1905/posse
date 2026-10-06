//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

type teardownFixtureProcess struct {
	PID       int
	BootID    string
	StartTime string
}

func finishTeardownOwnershipLifecycle(t *testing.T, mode, binary, repo, home, root string, env, leadEnv []string, db *store.DB, project store.Project, task store.Task) {
	t.Helper()
	scratch := filepath.Join(home, "scratch", "shop", "t1")
	if !waitForCondition(15*time.Second, func() bool { _, err := os.Stat(filepath.Join(root, "rider-ready")); return err == nil }) {
		t.Fatal("Rider did not finish its test process")
	}

	processes := map[string]teardownFixtureProcess{}
	processNames := []string{}
	switch mode {
	case "scratch-child":
		processNames = []string{"background", "global-child", "other-task-child"}
	case "local-ordering", "failed-discard-ordering":
		processNames = []string{"background"}
	}
	for _, name := range processNames {
		ready := filepath.Join(root, name+"-ready")
		if name == "background" {
			ready = filepath.Join(root, "background-ready")
		}
		if !waitForCondition(5*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }) {
			t.Fatalf("Rider's %s process did not start", name)
		}
		process := readTeardownFixtureProcess(t, root, name)
		processes[name] = process
		t.Cleanup(func() {
			_ = syscall.Kill(-process.PID, syscall.SIGKILL)
			_ = syscall.Kill(process.PID, syscall.SIGKILL)
		})
	}

	switch mode {
	case "local-ordering", "local-held-prune":
		if got := runPosse(t, binary, repo, leadEnv, "land", "t1", "--merge", "--user-approved", "User approved the isolated local Land"); !strings.Contains(got, "state: landed") {
			t.Fatalf("local Land did not leave a landed Task for explicit Teardown: %s", got)
		}
		if got := gitTest(t, env, repo, "show", "main:evidence.txt"); got != "evidence\n" {
			t.Fatalf("local Land lost delivered work: %q", got)
		}
		if mode == "local-held-prune" {
			mount, err := db.MountByTask(context.Background(), task.ID)
			if err != nil || mount.State != "held" {
				t.Fatalf("Land did not retain its Mount: %#v %v", mount, err)
			}
			before := strings.TrimSpace(gitTest(t, env, task.WorktreePath, "rev-parse", "HEAD"))
			dryRun := runPosse(t, binary, repo, leadEnv, "remuda", "prune")
			if strings.Contains(dryRun, task.Branch) {
				t.Fatalf("prune lists held Rider branch %s as reclaimable: %s", task.Branch, dryRun)
			}
			runPosse(t, binary, repo, leadEnv, "remuda", "prune", "--yes")
			after := strings.TrimSpace(gitTest(t, env, task.WorktreePath, "rev-parse", "HEAD"))
			if after != before {
				t.Fatalf("prune changed held Rider HEAD from %s to %s", before, after)
			}
			if current, err := db.MountByTask(context.Background(), task.ID); err != nil || current.State != "held" {
				t.Fatalf("prune changed the held Mount: %#v %v", current, err)
			}
			if _, err := gitCommand(env, repo, "show-ref", "--verify", "refs/heads/"+task.Branch); err != nil {
				t.Fatalf("prune deleted the checked-out branch: %v", err)
			}
			return
		}
		runPosse(t, binary, repo, leadEnv, "unsaddle", "t1")
	case "failed-discard-ordering":
		runPosse(t, binary, repo, leadEnv, "unsaddle", "t1", "--discard", "--user-approved", "User approved the isolated failed Rider discard")
		bundles, err := filepath.Glob(filepath.Join(home, "projects", "shop", "tasks", "t1", "discard", "*.bundle"))
		if err != nil || len(bundles) != 1 {
			t.Fatalf("failed discard lost approved tip bundle: %v %v", bundles, err)
		}
	case "scratch-child":
		runPosse(t, binary, repo, leadEnv, "unsaddle", "t1")
		if !waitForCondition(7*time.Second, func() bool { return !teardownFixtureProcessRunning(processes["background"]) }) {
			t.Fatalf("Rider-created scratch process survived Teardown: pid=%d cwd=%q", processes["background"].PID, processCWD(processes["background"].PID))
		}
		assertFixtureDescriptorClosed(t, processes["background"], "3", "open-cache")
		for _, name := range []string{"global-child", "other-task-child"} {
			process := processes[name]
			if !teardownFixtureProcessRunning(process) {
				t.Errorf("Teardown signalled process in unowned %s storage: pid=%d", name, process.PID)
			}
			assertFixtureDescriptorOpen(t, process, "4", "foreign-cache")
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
			t.Fatalf("Rider shutdown handler did not run before scratch cleanup: %q %v", contents, err)
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
		t.Errorf("Mount not clean: %s", got)
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
		t.Errorf("Teardown left the Rider cache descriptor open: %s -> %s", path, target)
	}
}

func assertFixtureDescriptorOpen(t *testing.T, process teardownFixtureProcess, fd, name string) {
	t.Helper()
	path := fmt.Sprintf("/proc/%d/fd/%s", process.PID, fd)
	target, err := os.Readlink(path)
	if err != nil || !strings.Contains(target, name) {
		t.Errorf("Teardown changed an unowned cache descriptor: %s -> %s err=%v", path, target, err)
	}
}
