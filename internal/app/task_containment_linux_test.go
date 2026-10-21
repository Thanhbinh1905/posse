//go:build linux

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

func TestTaskContainmentRetainsNewProcessOnlyWithIndependentOwnershipProof(t *testing.T) {
	root := t.TempDir()
	marker := fmt.Sprintf("containment-test-%d", os.Getpid())
	task := store.Task{ContainmentMode: "marker", ContainmentMarker: marker}
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	if err := retainTaskContainmentProcesses(authorization, task, []ownedProcessRoot{{path: root}}); err != nil {
		t.Fatal(err)
	}

	owned := exec.Command("sleep", "300")
	owned.Dir = root
	owned.Env = taskContainmentTestEnvironment(marker, filepath.Join(root, "owned-ready"), 0)
	owned.Stdout, owned.Stderr = io.Discard, io.Discard
	if err := owned.Start(); err != nil {
		t.Fatal(err)
	}
	ownedWaited := false
	defer func() {
		if !ownedWaited {
			_ = owned.Process.Kill()
			_ = owned.Wait()
		}
	}()
	if !waitForTaskContainmentCondition(5*time.Second, func() bool { return processHasTaskMarker(owned.Process.Pid, marker) }) {
		t.Fatal("new Task-owned process did not expose its marker")
	}
	if err := authorization.guardSignal(owned.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("retain and signal a new process with marker and Task-path proof: %v", err)
	}
	handle := authorization.handle(owned.Process.Pid)
	if handle == nil {
		t.Fatal("newly authorized Task process did not retain its exact process handle")
	}
	if err := handle.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal newly authorized Task process: %v", err)
	}
	if err := owned.Wait(); err == nil {
		t.Fatal("new Task-owned process exited without a signal")
	}
	ownedWaited = true

	foreign := exec.Command("sleep", "300")
	foreign.Dir = "/"
	foreign.Env = taskContainmentTestEnvironment(marker, filepath.Join(root, "foreign-ready"), 0)
	foreign.Stdout, foreign.Stderr = io.Discard, io.Discard
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	foreignWaited := false
	defer func() {
		if !foreignWaited {
			_ = foreign.Process.Kill()
			_ = foreign.Wait()
		}
	}()
	if !waitForTaskContainmentCondition(5*time.Second, func() bool { return processHasTaskMarker(foreign.Process.Pid, marker) }) {
		t.Fatal("foreign Task-marked process did not start")
	}
	if _, err := stopMarkedTaskProcesses(marker, authorization.guardSignal, nil, 0); err != nil {
		t.Fatalf("marker-only process without a Task-path reference blocked Task teardown: %v", err)
	}
	if err := foreign.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("marker-only process was signaled despite lacking Task-path proof: %v", err)
	}
}

func TestStopMarkedTaskProcessesStopsDetachedServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("containment-test-%d", os.Getpid())
	ready := filepath.Join(t.TempDir(), "ready")
	script := `import os, socket, time
os.setsid()
os.chdir("/")
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", int(os.environ["TASK_TEST_PORT"])))
s.listen()
open(os.environ["TASK_TEST_READY"], "w").write(str(os.getpid()))
while True:
    connection, _ = s.accept()
    connection.close()
`
	command := exec.Command("python3", "-c", script)
	command.Dir = "/"
	command.Env = taskContainmentTestEnvironment(marker, ready, port)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	if !waitForTaskContainmentFile(5*time.Second, ready) {
		t.Fatal("marked detached server did not start")
	}
	pidBytes, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid != command.Process.Pid {
		t.Fatalf("server PID = %q, command PID %d: %v", pidBytes, command.Process.Pid, err)
	}
	if connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err != nil {
		t.Fatalf("server did not accept connections: %v", err)
	} else {
		_ = connection.Close()
	}

	if _, err := stopTaskProcessContainment(context.Background(), store.Task{ContainmentMode: "marker", ContainmentMarker: marker}); err != nil {
		t.Fatalf("stop marker-owned process: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- command.Wait() }()
	select {
	case err := <-waitErr:
		waited = true
		if err == nil {
			t.Fatal("server exited without a signal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("marked server did not exit after process containment")
	}
	if connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 150*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatalf("server still listens on port %d after containment", port)
	}
}

func TestTaskProcessContainmentPreservesDetachedSSHControlMaster(t *testing.T) {
	root := t.TempDir()
	marker := fmt.Sprintf("containment-test-%d-%s", os.Getpid(), filepath.Base(root))
	pidPath := filepath.Join(root, "control-master.pid")
	t.Cleanup(func() {
		pids, err := markedTaskPIDs(marker)
		if err != nil {
			t.Errorf("find test-owned ControlMaster processes for cleanup: %v", err)
			return
		}
		for _, pid := range pids {
			handle, err := openSandboxProcessHandle(pid)
			if err != nil {
				continue
			}
			_ = handle.Signal(syscall.SIGKILL)
			_ = handle.Close()
		}
	})
	script := `import os, sys
pid = os.fork()
if pid:
    os._exit(0)
os.setsid()
pid = os.fork()
if pid:
    os._exit(0)
fd = os.open(sys.argv[1], os.O_CREAT | os.O_WRONLY, 0o600)
os.write(fd, str(os.getpid()).encode())
os.close(fd)
os.execv("/bin/bash", ["bash", "-c", "exec -a 'ssh -M -S " + sys.argv[1] + "' sleep 300"])
`
	command := exec.Command("python3", "-c", script, pidPath)
	command.Env = taskContainmentTestEnvironment(marker, pidPath, 0)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("launch detached fake ControlMaster: %v", err)
	}
	if !waitForTaskContainmentFile(5*time.Second, pidPath) {
		t.Fatal("detached fake ControlMaster did not write its PID")
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 1 {
		t.Fatalf("ControlMaster PID = %q: %v", pidBytes, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		processes := processDetails([]int{pid})
		if len(processes) == 1 && strings.Contains(processes[0].Command, "ssh -M -S") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake ControlMaster command did not exec: %#v", processes)
		}
		time.Sleep(20 * time.Millisecond)
	}
	handle, err := openSandboxProcessHandle(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if alive, _ := handle.Alive(); alive {
			_ = handle.Signal(syscall.SIGKILL)
		}
		_ = handle.Close()
	}()
	if !processHasTaskMarker(pid, marker) {
		t.Fatalf("detached ControlMaster PID %d lost its inherited Task marker", pid)
	}
	parent, err := sandboxProcessParentID(pid)
	if err != nil || parent > 1 && processHasTaskMarker(parent, marker) {
		t.Fatalf("ControlMaster PID %d still appears to have a Worker parent %d: %v", pid, parent, err)
	}

	ordinary := exec.Command("sleep", "300")
	ordinary.Env = taskContainmentTestEnvironment(marker, filepath.Join(root, "ordinary.pid"), 0)
	if err := ordinary.Start(); err != nil {
		t.Fatal(err)
	}
	ordinaryWaited := false
	defer func() {
		if !ordinaryWaited {
			_ = ordinary.Process.Kill()
			_ = ordinary.Wait()
		}
	}()
	ordinaryHandle, err := openSandboxProcessHandle(ordinary.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer ordinaryHandle.Close()

	preserved, err := stopTaskProcessContainment(context.Background(), store.Task{ContainmentMode: "marker", ContainmentMarker: marker})
	if err != nil {
		t.Fatalf("Task teardown stopped at the shared daemon instead of stopping other processes: %v", err)
	}
	if alive, err := ordinaryHandle.Alive(); err != nil || alive {
		t.Fatalf("ordinary Task process survived containment: alive=%t err=%v", alive, err)
	}
	if err := ordinary.Wait(); err == nil {
		t.Fatal("ordinary Task process exited without a signal")
	}
	ordinaryWaited = true
	alive, err := handle.Alive()
	if err != nil || !alive {
		t.Fatalf("detached ControlMaster was signalled: alive=%t err=%v", alive, err)
	}
	if len(preserved) != 1 || preserved[0].PID != pid || !strings.Contains(preserved[0].Command, "ssh -M -S") {
		t.Fatalf("preserved shared daemons = %#v, want PID %d with its command", preserved, pid)
	}
}

func TestInPlaceContainmentPreservesForeignForegroundProcess(t *testing.T) {
	root := t.TempDir()
	marker := fmt.Sprintf("containment-test-%d-%s", os.Getpid(), filepath.Base(root))
	command := exec.Command("sleep", "300")
	command.Dir = root
	command.Env = taskContainmentTestEnvironment(marker, filepath.Join(root, "unused"), 0)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	handle, err := openSandboxProcessHandle(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	defer func() {
		if alive, _ := handle.Alive(); alive {
			_ = handle.Signal(syscall.SIGKILL)
		}
		_ = handle.Close()
		_ = command.Wait()
	}()

	const paneID = "w1:p2"
	project := store.Project{ID: 1, Name: "local", Root: root, Kind: store.ProjectKindDirectory, HerdrWorkspaceID: "w1"}
	task := store.Task{ID: 2, Seq: 1, WorktreePath: root, HerdrWorkspaceID: "w1", PaneID: paneID, PaneLabel: "posse:local:t1", ContainmentMode: "marker", ContainmentMarker: marker}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: paneID, WorkspaceID: "w1", TabID: "w1:t2", Label: task.PaneLabel, CWD: root}}}
	processInfo, err := json.Marshal(map[string]any{"process_info": map[string]any{
		"pane_id": paneID, "shell_pid": os.Getpid(), "foreground_process_group_id": command.Process.Pid,
		"foreground_processes": []map[string]any{{"pid": command.Process.Pid, "name": "vim", "cwd": root}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake.Results["pane.process_info"] = processInfo
	service := testService(t.TempDir(), fake)
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	for _, pid := range []int{os.Getpid(), command.Process.Pid} {
		if _, err := authorization.bind(pid); err != nil {
			t.Fatal(err)
		}
	}
	_, err = stopTaskProcessContainmentWithGuard(context.Background(), task, func(pid int, _ syscall.Signal) error {
		return service.verifyInPlaceProcessSignalOwnership(context.Background(), project, task, pid, os.Getpid(), command.Process.Pid, authorization)
	})
	if err == nil || !strings.Contains(err.Error(), "foreign foreground process") {
		t.Fatalf("foreign pane foreground was not refused: %v", err)
	}
	alive, err := handle.Alive()
	if err != nil || !alive {
		t.Fatalf("foreign foreground PID %d was signalled: alive=%t err=%v", command.Process.Pid, alive, err)
	}
}

func TestInPlaceContainmentStopsMarkedProcessAfterPaneDisappears(t *testing.T) {
	root := t.TempDir()
	marker := fmt.Sprintf("containment-test-%d-%s", os.Getpid(), filepath.Base(root))
	command := exec.Command("sleep", "300")
	command.Dir = root
	command.Env = taskContainmentTestEnvironment(marker, filepath.Join(root, "unused"), 0)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !processHasTaskMarker(command.Process.Pid, marker) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !processHasTaskMarker(command.Process.Pid, marker) {
		t.Fatal("marked in-place process did not start")
	}

	project := store.Project{ID: 1, Name: "local", Root: root, Kind: store.ProjectKindDirectory, HerdrWorkspaceID: "w1"}
	task := store.Task{ID: 2, Seq: 1, WorktreePath: root, HerdrWorkspaceID: "w1", PaneID: "w1:p2", PaneLabel: "posse:local:t1", ContainmentMode: "marker", ContainmentMarker: marker}
	fake := herdr.NewFake()
	service := testService(t.TempDir(), fake)
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	if _, err := authorization.bind(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if _, err := stopTaskProcessContainmentWithGuard(context.Background(), task, func(pid int, _ syscall.Signal) error {
		return service.verifyInPlaceProcessSignalOwnership(context.Background(), project, task, pid, 0, 0, authorization)
	}); err != nil {
		t.Fatalf("in-place Teardown failed after the Task pane disappeared: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- command.Wait() }()
	select {
	case err := <-waitErr:
		waited = true
		if err == nil {
			t.Fatal("marked process exited without a signal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-place Teardown left the marker-owned process running after the pane disappeared")
	}
}

func TestTaskPaneShellGetsShortSIGKILLGrace(t *testing.T) {
	root := t.TempDir()
	marker := fmt.Sprintf("containment-test-%d-%s", os.Getpid(), filepath.Base(root))
	ready := filepath.Join(root, "ready")
	command := exec.Command("python3", "-c", `import signal,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); open(__import__("os").environ["TASK_TEST_READY"],"w").write(str(__import__("os").getpid())); time.sleep(300)`)
	command.Env = taskContainmentTestEnvironment(marker, ready, 0)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	if !waitForTaskContainmentFile(5*time.Second, ready) {
		t.Fatal("pane-shell fixture did not start")
	}
	started := time.Now()
	if _, err := stopMarkedTaskProcesses(marker, nil, nil, command.Process.Pid); err != nil {
		t.Fatalf("stop pane-shell fixture: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 1500*time.Millisecond {
		t.Fatalf("pane-shell SIGKILL phase took %s, want less than 1.5s", elapsed)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("pane-shell fixture exited without a signal")
	}
	waited = true
}

func TestTaskContainmentSignalsPaneShellLast(t *testing.T) {
	root := t.TempDir()
	marker := fmt.Sprintf("containment-test-%d-%s", os.Getpid(), filepath.Base(root))
	var commands []*exec.Cmd
	var pids []int
	for _, name := range []string{"other", "shell"} {
		command := exec.Command("sh", "-c", `echo $$ > "$1"; exec sleep 300`, "test", filepath.Join(root, name+".pid"))
		command.Env = taskContainmentTestEnvironment(marker, filepath.Join(root, name+".ready"), 0)
		if name == "shell" {
			filtered := make([]string, 0, len(command.Env))
			for _, entry := range command.Env {
				if !strings.HasPrefix(entry, "POSSE_TASK_ID=") {
					filtered = append(filtered, entry)
				}
			}
			command.Env = filtered
		}
		command.Stdout, command.Stderr = io.Discard, io.Discard
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
		defer func(command *exec.Cmd) {
			if command.ProcessState == nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
		}(command)
		pidPath := filepath.Join(root, name+".pid")
		if !waitForTaskContainmentFile(5*time.Second, pidPath) {
			t.Fatalf("%s process did not record its PID", name)
		}
		pidBytes, err := os.ReadFile(pidPath)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
		if err != nil || pid != command.Process.Pid {
			t.Fatalf("%s process PID = %q, command PID %d: %v", name, pidBytes, command.Process.Pid, err)
		}
		pids = append(pids, pid)
	}
	otherPID, shellPID := pids[0], pids[1]
	var signalOrder []int
	_, err := stopTaskProcessContainmentWithGuard(context.Background(), store.Task{ContainmentMode: "marker", ContainmentMarker: marker}, func(pid int, signal syscall.Signal) error {
		if signal == syscall.SIGTERM {
			signalOrder = append(signalOrder, pid)
		}
		return nil
	}, shellPID)
	if err != nil {
		t.Fatalf("stop pane processes: %v", err)
	}
	if len(signalOrder) != 2 || signalOrder[0] != otherPID || signalOrder[1] != shellPID {
		t.Fatalf("SIGTERM order = %v, want other process %d before pane shell %d", signalOrder, otherPID, shellPID)
	}
	for index, command := range commands {
		waited := make(chan error, 1)
		go func() { waited <- command.Wait() }()
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
			_ = command.Process.Kill()
			<-waited
			t.Fatalf("Task-owned process %d did not stop", pids[index])
		}
	}
}

func TestTaskContainmentSkipsReusedPaneShellPID(t *testing.T) {
	root := t.TempDir()
	marker := fmt.Sprintf("containment-test-%d", os.Getpid())
	command := exec.Command("sleep", "300")
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	handle, err := openSandboxProcessHandle(command.Process.Pid)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	defer func() {
		if alive, _ := handle.Alive(); alive {
			_ = handle.Signal(syscall.SIGKILL)
		}
		_ = handle.Close()
		_ = command.Wait()
	}()

	project := store.Project{ID: 1, Name: "local", Root: root, Kind: store.ProjectKindDirectory, HerdrWorkspaceID: "w1"}
	task := store.Task{ID: 2, Seq: 1, WorktreePath: root, HerdrWorkspaceID: "w1", PaneID: "w1:p2", PaneLabel: "posse:local:t1", ContainmentMode: "cgroup", ContainmentMarker: marker}
	service := testService(t.TempDir(), herdr.NewFake())
	_, err = stopTaskProcessContainmentWithGuard(context.Background(), task, func(pid int, signal syscall.Signal) error {
		return service.verifyInPlaceProcessSignalOwnership(context.Background(), project, task, pid, command.Process.Pid, 0, nil)
	}, command.Process.Pid)
	if err != nil {
		t.Fatalf("Teardown blocked on an unmarked process reusing a stale pane shell PID: %v", err)
	}
	alive, err := handle.Alive()
	if err != nil || !alive {
		t.Fatalf("Teardown signalled the unrelated reused PID %d: alive=%t err=%v", command.Process.Pid, alive, err)
	}
}

func TestCgroupModeStopsMarkedProcessOutsideCgroup(t *testing.T) {
	marker := fmt.Sprintf("containment-test-%d", os.Getpid())
	ready := filepath.Join(t.TempDir(), "ready")
	command := exec.Command("sh", "-c", `printf '%s' "$$" > "$TASK_TEST_READY"; exec sleep 60`)
	command.Env = taskContainmentTestEnvironment(marker, ready, 0)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	if !waitForTaskContainmentFile(5*time.Second, ready) {
		t.Fatal("marked process did not start")
	}
	pidBytes, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid != command.Process.Pid {
		t.Fatalf("marked process PID = %q, command PID %d: %v", pidBytes, command.Process.Pid, err)
	}
	task := store.Task{ContainmentMode: "cgroup", ContainmentMarker: marker}
	found := false
	for _, process := range taskContainmentProcessDetails(task) {
		if process.PID == pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("cgroup containment details omitted marked process %d outside the cgroup", pid)
	}
	if _, err := stopTaskProcessContainment(context.Background(), task); err != nil {
		t.Fatalf("stop marked process outside cgroup: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- command.Wait() }()
	select {
	case err := <-waitErr:
		waited = true
		if err == nil {
			t.Fatal("marked process exited without a signal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cgroup mode did not stop its marked out-of-cgroup process")
	}
}

func waitForTaskContainmentCondition(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return condition()
}

func waitForTaskContainmentFile(timeout time.Duration, path string) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if contents, err := os.ReadFile(path); err == nil && len(contents) > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func taskContainmentTestEnvironment(marker, ready string, port int) []string {
	result := []string{}
	taskTemp := filepath.Dir(ready)
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && key != "POSSE_TASK_ID" && key != "TASK_TEST_PORT" && key != "TASK_TEST_READY" {
			result = append(result, entry)
		}
	}
	return append(result,
		"TMPDIR="+taskTemp,
		"GOTMPDIR="+taskTemp,
		"TMP="+taskTemp,
		"TEMP="+taskTemp,
		"POSSE_TASK_TEMP="+taskTemp,
		"POSSE_TASK_ID="+marker,
		"TASK_TEST_READY="+ready,
		"TASK_TEST_PORT="+strconv.Itoa(port),
	)
}
