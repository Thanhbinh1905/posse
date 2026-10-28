//go:build linux

package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

func TestTaskCgroupMemberDoesNotNeedTaskMarker(t *testing.T) {
	pid := os.Getpid()
	current := processCgroup(pid)
	if current == "" {
		t.Skip("the test process is not in a unified cgroup")
	}
	cgroupPath := filepath.Join(cgroupV2Root, strings.TrimPrefix(current, string(filepath.Separator)))
	marker := strings.Repeat("a", 48)
	if processHasTaskMarker(pid, marker) {
		t.Fatalf("test process unexpectedly carries marker %q", marker)
	}
	if !taskCgroupMemberBelongsToTask(pid, 0, cgroupPath) {
		t.Fatalf("process %d belongs to cgroup %s but was rejected without marker %q", pid, cgroupPath, marker)
	}
}

// A leaked Task process whose leader exited while another thread runs keeps
// its zombie leader in the Task cgroup with no executable image to inspect.
func TestStopTaskCgroupStopsZombieLeaderWithLiveThread(t *testing.T) {
	task, cgroupFD := createTestTaskCgroup(t)
	command := exec.Command("python3")
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: cgroupFD}
	exitLeader := startZombieLeaderTestProcess(t, command)
	pid := command.Process.Pid
	if !processInCgroup(pid, task.ContainmentPath) {
		t.Fatalf("test process %d did not start in Task cgroup %s", pid, task.ContainmentPath)
	}
	exitLeader()

	if err := stopTaskCgroupWithGuard(context.Background(), task, nil, nil, 0); err != nil {
		t.Fatalf("stop Task cgroup with a zombie leader: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- command.Wait() }()
	select {
	case <-waitErr:
	case <-time.After(5 * time.Second):
		t.Fatal("zombie leader's thread group survived Task cgroup shutdown")
	}
	if _, err := os.Stat(task.ContainmentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("emptied Task cgroup was not removed: %v", err)
	}
}

// A Task process that is mid-exit when inspected has no executable image yet
// is still listed in the cgroup; the next pass sees it gone or stops it.
func TestStopTaskCgroupRetriesProcessThatExitsBeforeInspection(t *testing.T) {
	task, cgroupFD := createTestTaskCgroup(t)
	command := exec.Command("sleep", "60")
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: cgroupFD}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	pid := command.Process.Pid
	failed := false
	original := openCgroupProcessHandle
	openCgroupProcessHandle = func(candidate int) (*sandboxProcessHandle, error) {
		if candidate == pid && !failed {
			failed = true
			return nil, fmt.Errorf("inspect executable image for PID %d: %w", pid, &os.PathError{Op: "stat", Path: "/proc/exe", Err: syscall.ENOENT})
		}
		return original(candidate)
	}
	t.Cleanup(func() { openCgroupProcessHandle = original })

	if err := stopTaskCgroupWithGuard(context.Background(), task, nil, nil, 0); err != nil {
		t.Fatalf("stop Task cgroup with a process that vanished before inspection: %v", err)
	}
	if !failed {
		t.Fatal("the inspection failure was never injected")
	}
}

// createTestTaskCgroup creates an empty Task cgroup under the user's delegated
// app.slice and returns its Task with an open directory descriptor for it.
func createTestTaskCgroup(t *testing.T) (store.Task, int) {
	t.Helper()
	if !taskCgroupsAvailable(context.Background()) {
		t.Skip("delegated user cgroups are unavailable")
	}
	base, err := userAppSlicePath(context.Background())
	if err != nil {
		t.Skipf("user app.slice is unavailable: %v", err)
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	task := store.Task{ID: 1 << 30, ContainmentMode: "cgroup", ContainmentMarker: hex.EncodeToString(random)}
	task.ContainmentPath = filepath.Join(base, "posse-task-t1073741824-"+task.ContainmentMarker[:12]+".scope")
	if err := validateTaskCgroupPath(task.ContainmentPath, task); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(task.ContainmentPath, 0o755); err != nil {
		t.Skipf("cannot create a Task cgroup: %v", err)
	}
	directory, err := os.Open(task.ContainmentPath)
	if err != nil {
		_ = os.Remove(task.ContainmentPath)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = directory.Close()
		_ = os.WriteFile(filepath.Join(task.ContainmentPath, "cgroup.kill"), []byte("1"), 0)
		waitForTaskContainmentCondition(5*time.Second, func() bool {
			err := os.Remove(task.ContainmentPath)
			return err == nil || errors.Is(err, os.ErrNotExist)
		})
	})
	// Creating a cgroup does not prove the right to move processes into it,
	// which needs write access to the common ancestor's cgroup.procs.
	probe := exec.Command("true")
	probe.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(directory.Fd())}
	if err := probe.Run(); errors.Is(err, os.ErrPermission) {
		t.Skipf("cannot start a process in a Task cgroup: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	return task, int(directory.Fd())
}
