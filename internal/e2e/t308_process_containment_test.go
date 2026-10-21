//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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

func e2eProcessInTaskCgroup(pid int, path string) bool {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return false
	}
	root := strings.TrimPrefix(path, "/sys/fs/cgroup")
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(line, "0::") {
			current := strings.TrimPrefix(line, "0::")
			return current == root || strings.HasPrefix(current, strings.TrimSuffix(root, "/")+"/")
		}
	}
	return false
}

func TestTeardownStopsDetachedTaskServer(t *testing.T) {
	f := newWorkerTabsFixture(t)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	serverScript := fmt.Sprintf(`import os, socket, sys, time
root = sys.argv[1]
reference = open(sys.argv[2], "rb")
os.chdir("/")
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", %d))
s.listen()
open(os.path.join(root, "detached-server.ready"), "w").close()
while True:
    connection, _ = s.accept()
    connection.close()
`, port)
	if err := os.WriteFile(filepath.Join(f.root, "detached-server.py"), []byte(serverScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "launch-detached-server"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "check-task-env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	processes := map[string]teardownFixtureProcess{}
	t.Cleanup(func() { cleanupTeardownFixtureProcesses(t, f.root, []string{"detached-server"}, processes) })
	task := f.start(t, "t1", "Detached Worker server", "detached-worker-server")
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "detached-server.ready"))
		return err == nil
	}) {
		t.Fatal("Worker's detached server did not start")
	}
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "task-environment"))
		return err == nil
	}) {
		t.Fatal("fake Worker did not record its Task environment")
	}
	environment, err := os.ReadFile(filepath.Join(f.root, "task-environment"))
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Split(strings.TrimSpace(string(environment)), "\t")
	expectedScratch := filepath.Join(f.home, "scratch", "shop", fmt.Sprintf("t%d", task.Seq))
	expectedCache := filepath.Join(f.home, "cache", "worker-go-build")
	if len(values) != 7 || values[0] != expectedScratch || values[1] != expectedScratch || values[2] != expectedScratch || values[3] != expectedScratch || values[4] != expectedScratch || values[5] != task.ContainmentMarker || values[6] != expectedCache {
		t.Fatalf("Worker Task environment = %#v, Task = %#v; want every temp variable set to %s, the recorded marker, and shared Posse GOCACHE %s", values, task, expectedScratch, expectedCache)
	}
	if task.ContainmentMarker == "" {
		t.Fatalf("Task process containment identity = %#v", task)
	}
	if task.ContainmentMode != "marker" && (task.ContainmentMode != "cgroup" || task.ContainmentPath == "") {
		t.Fatalf("Task process containment mode/path = %q/%q", task.ContainmentMode, task.ContainmentPath)
	}
	pidBytes, err := os.ReadFile(filepath.Join(f.root, "detached-server.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 1 {
		t.Fatalf("detached server PID = %q: %v", pidBytes, err)
	}
	if task.ContainmentMode == "cgroup" && !e2eProcessInTaskCgroup(pid, task.ContainmentPath) {
		t.Fatalf("detached server PID %d is outside recorded Task cgroup %s", pid, task.ContainmentPath)
	}
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		t.Fatal(err)
	}
	processes["detached-server"] = teardownFixtureProcess{PID: pid, BootID: bootID, StartTime: startTime}
	if connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err != nil {
		t.Fatalf("detached server did not accept connections before Teardown: %v", err)
	} else {
		_ = connection.Close()
	}
	if filepath.Clean(processCWD(pid)) != "/" {
		t.Fatalf("detached server kept a Sandbox working directory: %q", processCWD(pid))
	}

	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("Teardown failed: %s", output)
	}
	if !waitForCondition(7*time.Second, func() bool { return !teardownFixtureProcessRunning(processes["detached-server"]) }) {
		t.Fatalf("setsid server survived Teardown: pid=%d cwd=%q", pid, processCWD(pid))
	}
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("Teardown left the Worker's detached server listening on port %d", port)
	}
	if current := f.task(t, "t1"); current.State != store.StateTornDown {
		t.Fatalf("Task after Teardown = %s", current.State)
	}
}

func TestTeardownPreservesDetachedSSHControlMaster(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Detached SSH ControlMaster", "ssh-control-master")
	pidPath := filepath.Join(f.root, "ssh-control-master.pid")
	childPIDPath := filepath.Join(f.root, "ssh-control-child.pid")
	masterScriptPath := filepath.Join(f.root, "ssh-control-master.sh")
	ordinaryPIDPath := filepath.Join(f.root, "ordinary-task-process.pid")
	socketPath := filepath.Join(f.root, "ssh-control.sock")
	var master, child, ordinary teardownFixtureProcess
	t.Cleanup(func() {
		for _, process := range []teardownFixtureProcess{master, child, ordinary} {
			if teardownFixtureProcessRunning(process) {
				_ = syscall.Kill(process.PID, syscall.SIGKILL)
			}
		}
	})
	raw, err := f.client.Call(context.Background(), "tab.create", map[string]any{
		"workspace_id": task.HerdrWorkspaceID,
		"cwd":          task.WorktreePath,
		"label":        "e2e-ssh-control-master",
		"focus":        false,
		"env":          map[string]string{"POSSE_TASK_ID": task.ContainmentMarker},
	})
	if err != nil {
		t.Fatalf("create isolated Herdr shell tab: %v", err)
	}
	var shellTab workspaceResult
	if err := json.Unmarshal(raw, &shellTab); err != nil || shellTab.RootPane.PaneID == "" {
		t.Fatalf("Herdr shell tab response = %s: %v", raw, err)
	}
	masterScript := `#!/usr/bin/env bash
echo $$ > "$1"
exec -a "ssh -M -S $2" bash -c 'sleep 300 & echo $! > "$1"; wait' e2e-control-child "$3"
`
	if err := os.WriteFile(masterScriptPath, []byte(masterScript), 0o700); err != nil {
		t.Fatal(err)
	}
	command := `setsid bash ` + shellQuote(masterScriptPath) + ` ` + shellQuote(pidPath) + ` ` + shellQuote(socketPath) + ` ` + shellQuote(childPIDPath) + ` >/dev/null 2>&1 </dev/null &`
	if _, err := f.client.Run(context.Background(), "pane", "run", shellTab.RootPane.PaneID, command); err != nil {
		t.Fatalf("start detached fake ControlMaster through Herdr: %v", err)
	}
	ordinaryCommand := `setsid bash -c 'echo $$ > "$1"; exec sleep 300' e2e-ordinary ` + shellQuote(ordinaryPIDPath) + ` >/dev/null 2>&1 </dev/null &`
	if _, err := f.client.Run(context.Background(), "pane", "run", shellTab.RootPane.PaneID, ordinaryCommand); err != nil {
		t.Fatalf("start ordinary detached Task process through Herdr: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(pidPath)
		return err == nil && len(strings.TrimSpace(string(contents))) > 0
	}) {
		t.Fatal("detached fake ControlMaster did not record its PID")
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 1 {
		t.Fatalf("ControlMaster PID = %q: %v", pidBytes, err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(childPIDPath)
		return err == nil && len(strings.TrimSpace(string(contents))) > 0
	}) {
		t.Fatal("detached ControlMaster child did not record its PID")
	}
	childPIDBytes, err := os.ReadFile(childPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(childPIDBytes)))
	if err != nil || childPID <= 1 {
		t.Fatalf("ControlMaster child PID = %q: %v", childPIDBytes, err)
	}
	childBootID, childStartTime, err := store.ProcessIdentityForPID(childPID)
	if err != nil {
		t.Fatal(err)
	}
	child = teardownFixtureProcess{PID: childPID, BootID: childBootID, StartTime: childStartTime}
	if !waitForCondition(5*time.Second, func() bool {
		contents, err := os.ReadFile(ordinaryPIDPath)
		return err == nil && len(strings.TrimSpace(string(contents))) > 0
	}) {
		t.Fatal("ordinary detached Task process did not record its PID")
	}
	ordinaryPIDBytes, err := os.ReadFile(ordinaryPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryPID, err := strconv.Atoi(strings.TrimSpace(string(ordinaryPIDBytes)))
	if err != nil || ordinaryPID <= 1 {
		t.Fatalf("ordinary Task process PID = %q: %v", ordinaryPIDBytes, err)
	}
	ordinaryBootID, ordinaryStartTime, err := store.ProcessIdentityForPID(ordinaryPID)
	if err != nil {
		t.Fatal(err)
	}
	ordinary = teardownFixtureProcess{PID: ordinaryPID, BootID: ordinaryBootID, StartTime: ordinaryStartTime}
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		t.Fatal(err)
	}
	master = teardownFixtureProcess{PID: pid, BootID: bootID, StartTime: startTime}
	if task.ContainmentMode == "cgroup" && e2eProcessInTaskCgroup(pid, task.ContainmentPath) {
		t.Fatalf("detached ControlMaster unexpectedly inherited Task cgroup %s", task.ContainmentPath)
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil || !strings.Contains(string(environ), "POSSE_TASK_ID="+task.ContainmentMarker) {
		t.Fatalf("detached ControlMaster lost the Task marker: %q: %v", environ, err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		commandLine, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		return err == nil && strings.Contains(string(commandLine), "ssh -M -S "+socketPath)
	}) {
		t.Fatal("detached fake ControlMaster did not exec with its SSH ControlMaster command line")
	}
	if _, err := f.client.Call(context.Background(), "tab.close", map[string]any{"tab_id": shellTab.RootPane.TabID}); err != nil {
		t.Fatalf("close the temporary Herdr shell tab: %v", err)
	}
	if !waitForCondition(3*time.Second, func() bool { return teardownFixtureProcessRunning(master) }) {
		t.Fatalf("detached ControlMaster %d did not survive its shell tab", pid)
	}
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")
	teardown := exec.Command(f.binary, "teardown", "t1")
	teardown.Dir, teardown.Env = f.repo, f.leadEnv
	outputBytes, teardownErr := teardown.CombinedOutput()
	output := string(outputBytes)
	if teardownErr == nil || !strings.Contains(output, "cannot release the Sandbox while live processes still reference it") || !strings.Contains(output, strconv.Itoa(pid)) {
		t.Fatalf("Teardown did not block release for ControlMaster PID %d (child %d, ordinary process %d): err=%v output=%s", pid, childPID, ordinaryPID, teardownErr, output)
	}
	if !teardownFixtureProcessRunning(master) {
		t.Fatalf("Teardown signalled detached ControlMaster PID %d", pid)
	}
	if !teardownFixtureProcessRunning(child) {
		t.Fatalf("Teardown signalled descendant %d of detached ControlMaster %d", childPID, pid)
	}
	if !waitForCondition(3*time.Second, func() bool { return !teardownFixtureProcessRunning(ordinary) }) {
		t.Fatalf("Teardown preserved ordinary Task process PID %d along with the shared daemon", ordinaryPID)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), task.ProjectID, false)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	blockedNotice := false
	for _, notice := range notices {
		blockedNotice = blockedNotice || notice.TaskID == task.ID && notice.Kind == "teardown_blocked" && strings.Contains(notice.Summary, strconv.Itoa(pid))
	}
	if !blockedNotice {
		t.Fatalf("Teardown did not publish a PID-specific blocked Notice: %#v", notices)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		t.Fatalf("stop test-owned detached ControlMaster: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool { return !teardownFixtureProcessRunning(master) }) {
		t.Fatalf("test-owned ControlMaster %d did not exit", pid)
	}
	if output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1"); !strings.Contains(output, "torn-down") {
		t.Fatalf("Teardown did not finish after the shared daemon exited: %s", output)
	}
	if current := f.task(t, "t1"); current.State != store.StateTornDown {
		t.Fatalf("Task after retry = %s", current.State)
	}
	if teardownFixtureProcessRunning(child) {
		t.Fatalf("Teardown left descendant %d of the exited ControlMaster running", childPID)
	}
}

func TestTeardownStopsProcessStartedThroughHerdrServer(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Herdr server process", "herdr-server-process")
	lockPath := filepath.Join(f.root, "posse.db.sandbox-lock")
	readyPath := filepath.Join(f.root, "herdr-lock-owner.ready")
	pidPath := filepath.Join(f.root, "herdr-lock-owner.pid")
	var owner teardownFixtureProcess
	t.Cleanup(func() {
		if owner.PID > 1 && teardownFixtureProcessRunning(owner) {
			_ = syscall.Kill(owner.PID, syscall.SIGKILL)
		}
	})
	raw, err := f.client.Call(context.Background(), "tab.create", map[string]any{
		"workspace_id": task.HerdrWorkspaceID,
		"cwd":          task.WorktreePath,
		"label":        "e2e-lock-owner",
		"focus":        false,
		"env":          map[string]string{"POSSE_TASK_ID": task.ContainmentMarker},
	})
	if err != nil {
		t.Fatalf("create isolated Herdr shell tab: %v", err)
	}
	var testTab workspaceResult
	if err := json.Unmarshal(raw, &testTab); err != nil || testTab.RootPane.PaneID == "" {
		t.Fatalf("Herdr shell tab response = %s: %v", raw, err)
	}
	command := `setsid sh -c 'exec 9>"$1"; flock -x 9; printf locked > "$2"; echo $$ > "$3"; exec sleep 60' lock-owner ` + shellQuote(lockPath) + ` ` + shellQuote(readyPath) + ` ` + shellQuote(pidPath) + ` >/dev/null 2>&1 </dev/null &`
	if _, err := f.client.Run(context.Background(), "pane", "run", testTab.RootPane.PaneID, command); err != nil {
		t.Fatalf("start lock owner through Herdr server: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}) {
		t.Fatal("Herdr server did not start the lock owner")
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 1 {
		t.Fatalf("Herdr lock-owner PID = %q: %v", pidBytes, err)
	}
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		t.Fatalf("read Herdr lock-owner identity: %v", err)
	}
	owner = teardownFixtureProcess{PID: pid, BootID: bootID, StartTime: startTime}
	if filepath.Clean(processCWD(pid)) != filepath.Clean(task.WorktreePath) {
		t.Fatalf("Herdr-launched lock owner cwd = %q, want isolated Task Sandbox %q", processCWD(pid), task.WorktreePath)
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil || !strings.Contains(string(environ), "POSSE_TASK_ID="+task.ContainmentMarker) {
		t.Fatalf("Herdr-launched lock owner lost the Task marker: %q: %v", environ, err)
	}
	if task.ContainmentMode == "cgroup" && e2eProcessInTaskCgroup(pid, task.ContainmentPath) {
		t.Fatalf("Herdr-launched lock owner unexpectedly inherited Task cgroup %s", task.ContainmentPath)
	}
	if _, err := f.client.Call(context.Background(), "tab.close", map[string]any{"tab_id": testTab.RootPane.TabID}); err != nil {
		t.Fatalf("close the test-created shell tab: %v", err)
	}
	if !waitForCondition(3*time.Second, func() bool { return teardownFixtureProcessRunning(owner) }) {
		t.Fatalf("Herdr lock owner %d did not survive its shell tab", pid)
	}
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("Teardown failed for Herdr-launched process %d: %s", pid, output)
	}
	if waitForCondition(2*time.Second, func() bool { return teardownFixtureProcessRunning(owner) }) {
		t.Fatalf("Teardown left Herdr-launched lock owner %d running", pid)
	}
}
