//go:build linux

package app

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

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

func waitForTaskContainmentFile(timeout time.Duration, path string) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
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
