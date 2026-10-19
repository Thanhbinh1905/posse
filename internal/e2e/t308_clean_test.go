//go:build e2e && linux

package e2e

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
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTeardownBlockedNoticeNamesForeignMountServer(t *testing.T) {
	f := newRiderTabsFixture(t)
	task := f.ride(t, "t1", "Foreign Mount server", "foreign-mount-server")
	markTaskReported(t, f, task)
	f.exitRider(t, "t1")

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	serverScript := `import os, socket, sys
os.setsid()
root, cwd, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
os.chdir(cwd)
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", port))
s.listen()
open(os.path.join(root, "foreign-server.pid"), "w").write(str(os.getpid()))
while True:
    connection, _ = s.accept()
    connection.close()
`
	command := exec.Command("python3", "-c", serverScript, f.root, task.WorktreePath, strconv.Itoa(port))
	command.Dir = task.WorktreePath
	command.Env = setEnv(f.env, "POSSE_TASK_ID", "")
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	processes := map[string]teardownFixtureProcess{}
	t.Cleanup(func() {
		cleanupTeardownFixtureProcesses(t, f.root, []string{"foreign-server"}, processes)
		if len(processes) == 0 && command.Process != nil {
			_ = command.Process.Kill()
		}
		_, _ = command.Process.Wait()
	})
	pidPath := filepath.Join(f.root, "foreign-server.pid")
	if !waitForCondition(5*time.Second, func() bool {
		_, err := os.Stat(pidPath)
		return err == nil
	}) {
		t.Fatal("foreign Mount server did not start")
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid != command.Process.Pid {
		t.Fatalf("foreign server PID = %q, command PID %d: %v", pidBytes, command.Process.Pid, err)
	}
	bootID, startTime, err := store.ProcessIdentityForPID(pid)
	if err != nil {
		t.Fatal(err)
	}
	processes["foreign-server"] = teardownFixtureProcess{PID: pid, BootID: bootID, StartTime: startTime}

	teardown := exec.Command(f.binary, "unsaddle", "t1")
	teardown.Dir, teardown.Env = f.repo, f.leadEnv
	output, teardownErr := teardown.CombinedOutput()
	if teardownErr == nil {
		t.Fatalf("Teardown unexpectedly succeeded with foreign Mount server PID %d: %s", pid, output)
	}
	if !strings.Contains(string(output), "unsaddle_incomplete") {
		t.Fatalf("Teardown failure was not reported: %v\n%s", teardownErr, output)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	if err != nil {
		t.Fatal(err)
	}
	var blocked *store.Notice
	for index := range notices {
		if notices[index].TaskID == task.ID && notices[index].Kind == "teardown_blocked" {
			blocked = &notices[index]
		}
	}
	if blocked == nil || !strings.Contains(blocked.Summary, strconv.Itoa(pid)) || !strings.Contains(blocked.Summary, strconv.Itoa(port)) || !strings.Contains(blocked.Summary, "python3") {
		t.Fatalf("teardown_blocked Notice did not identify PID, command and listening port: %#v", blocked)
	}
	mount, mountErr := db.MountByTask(context.Background(), task.ID)
	_ = db.Close()
	if mountErr != nil || mount.State != "held" {
		t.Fatalf("foreign process did not preserve the held Mount: %#v, %v", mount, mountErr)
	}
	if !teardownFixtureProcessRunning(processes["foreign-server"]) {
		t.Fatal("Teardown signalled the foreign process")
	}
	var doctor struct {
		Checks []struct {
			Check  string `json:"check"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	doctorOutput := runPosse(t, f.binary, f.repo, f.leadEnv, "doctor", "--json")
	if err := json.Unmarshal([]byte(doctorOutput), &doctor); err != nil {
		t.Fatalf("decode doctor report: %v: %s", err, doctorOutput)
	}
	leakReported := false
	for _, check := range doctor.Checks {
		if check.Check == "Task temp and process leaks shop" {
			leakReported = check.Status == "warn" && strings.Contains(check.Detail, strconv.Itoa(pid)) && strings.Contains(check.Detail, strconv.Itoa(port))
		}
	}
	if !leakReported {
		t.Fatalf("doctor did not report the residual PID and listening port: %s", doctorOutput)
	}
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("foreign server stopped after blocked Teardown: %v", err)
	}
	_ = connection.Close()
}

func TestCleanDryRunAndReclaimsFinishedTask(t *testing.T) {
	f := newRiderTabsFixture(t)
	task := f.ride(t, "t1", "Clean finished Task", "clean-finished-task")
	scratch := filepath.Join(f.home, "scratch", "shop", fmt.Sprintf("t%d", task.Seq))
	if err := os.WriteFile(filepath.Join(scratch, "clean-me.tmp"), []byte("task temp data"), 0o600); err != nil {
		t.Fatal(err)
	}
	markTaskReported(t, f, task)
	f.exitRider(t, "t1")

	var preview struct {
		DryRun bool `json:"dry_run"`
		Clean  []struct {
			Task        string `json:"task"`
			ScratchSize int64  `json:"scratch_bytes"`
			MountState  string `json:"mount_state"`
			Result      string `json:"result"`
		} `json:"clean"`
	}
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "clean", "--dry-run", "--json")
	if err := json.Unmarshal([]byte(output), &preview); err != nil {
		t.Fatalf("decode clean dry-run output: %v: %s", err, output)
	}
	if !preview.DryRun || len(preview.Clean) != 1 || preview.Clean[0].Task != "t1" || preview.Clean[0].ScratchSize == 0 || preview.Clean[0].MountState != "held" || preview.Clean[0].Result != "dry_run" {
		t.Fatalf("clean dry run did not describe the finished Task: %#v", preview)
	}
	if current := f.task(t, "t1"); current.State != store.StateReported {
		t.Fatalf("dry run changed Task state to %s", current.State)
	}
	if _, err := os.Stat(filepath.Join(scratch, "clean-me.tmp")); err != nil {
		t.Fatalf("dry run removed Task scratch: %v", err)
	}
	var doctor struct {
		Checks []struct {
			Check  string `json:"check"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	output = runPosse(t, f.binary, f.repo, f.leadEnv, "doctor", "--json")
	if err := json.Unmarshal([]byte(output), &doctor); err != nil {
		t.Fatalf("decode doctor output: %v: %s", err, output)
	}
	foundStorageReport := false
	for _, check := range doctor.Checks {
		if check.Check == "Task temp and process leaks shop" {
			foundStorageReport = strings.Contains(check.Detail, "t1=") && strings.Contains(check.Detail, "bytes in 1 Task scratch directory")
		}
	}
	if !foundStorageReport {
		t.Fatalf("doctor omitted per-Task scratch reporting: %s", output)
	}

	var result struct {
		DryRun bool `json:"dry_run"`
		Clean  []struct {
			Task   string `json:"task"`
			Result string `json:"result"`
		} `json:"clean"`
	}
	output = runPosse(t, f.binary, f.repo, f.leadEnv, "clean", "--yes", "--json")
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode clean result: %v: %s", err, output)
	}
	if result.DryRun || len(result.Clean) != 1 || result.Clean[0].Task != "t1" || result.Clean[0].Result != "cleaned" {
		t.Fatalf("clean did not reclaim the finished Task: %#v", result)
	}
	if current := f.task(t, "t1"); current.State != store.StateTornDown {
		t.Fatalf("Task after clean = %s", current.State)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("clean left Task scratch in place: %v", err)
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := db.Mounts(context.Background(), f.projectID)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range mounts {
		if mount.TaskID == task.ID && mount.State != "idle" {
			t.Fatalf("Mount after clean = %#v", mount)
		}
	}
}
