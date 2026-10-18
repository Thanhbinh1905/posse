//go:build linux

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
