//go:build e2e && linux

package e2e

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	f := newRiderTabsFixture(t)
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
	task := f.ride(t, "t1", "Detached Rider server", "detached-rider-server")
	if !waitForCondition(10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "detached-server.ready"))
		return err == nil
	}) {
		t.Fatal("Rider's detached server did not start")
	}
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "task-environment"))
		return err == nil
	}) {
		t.Fatal("fake Rider did not record its Task environment")
	}
	environment, err := os.ReadFile(filepath.Join(f.root, "task-environment"))
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Split(strings.TrimSpace(string(environment)), "\t")
	expectedScratch := filepath.Join(f.home, "scratch", "shop", fmt.Sprintf("t%d", task.Seq))
	if len(values) != 6 || values[0] != expectedScratch || values[1] != expectedScratch || values[2] != expectedScratch || values[3] != expectedScratch || values[4] != expectedScratch || values[5] != task.ContainmentMarker {
		t.Fatalf("Rider Task environment = %#v, Task = %#v; want every temp variable set to %s and the recorded marker", values, task, expectedScratch)
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
		t.Fatalf("detached server kept a Mount working directory: %q", processCWD(pid))
	}

	markTaskReported(t, f, task)
	f.exitRider(t, "t1")
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "unsaddle", "t1")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("Teardown failed: %s", output)
	}
	if !waitForCondition(7*time.Second, func() bool { return !teardownFixtureProcessRunning(processes["detached-server"]) }) {
		t.Fatalf("setsid server survived Teardown: pid=%d cwd=%q", pid, processCWD(pid))
	}
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("Teardown left the Rider's detached server listening on port %d", port)
	}
	if current := f.task(t, "t1"); current.State != store.StateTornDown {
		t.Fatalf("Task after Teardown = %s", current.State)
	}
}
