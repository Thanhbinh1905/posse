//go:build e2e && linux

package e2e

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

var transientChild = regexp.MustCompile(`PID \d+ (sleep|unknown command)`)

// A residual polling shell loop spawns a short-lived `sleep` child on every
// iteration. Retried Teardowns must keep one teardown_blocked Notice naming
// the shell, never the transient children.
func TestTeardownBlockedNoticeIgnoresTransientChildren(t *testing.T) {
	f := newWorkerTabsFixture(t)
	task := f.start(t, "t1", "Residual polling loop", "residual-polling-loop")
	markTaskReported(t, f, task)
	f.exitWorker(t, "t1")

	stopPath := filepath.Join(f.root, "stop-residual-loop")
	loop := exec.Command("/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.1; done`, "sh", stopPath)
	loop.Dir = task.WorktreePath
	loop.Env = setEnv(f.env, "POSSE_TASK_ID", "")
	loop.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	loop.Stdout, loop.Stderr = io.Discard, io.Discard
	if err := loop.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(stopPath, []byte("stop\n"), 0o600)
		_ = loop.Process.Kill()
		_, _ = loop.Process.Wait()
	})
	loopPID := loop.Process.Pid
	// The loop predates Teardown by a wide margin.
	time.Sleep(300 * time.Millisecond)

	for attempt := 1; attempt <= 6; attempt++ {
		teardown := exec.Command(f.binary, "teardown", "t1")
		teardown.Dir, teardown.Env = f.repo, f.leadEnv
		output, err := teardown.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "teardown_incomplete") {
			t.Fatalf("attempt %d: Teardown should be blocked by the residual loop: err=%v\n%s", attempt, err, output)
		}
	}

	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	blocked, incomplete := 0, 0
	for _, notice := range notices {
		if notice.TaskID != task.ID {
			continue
		}
		switch notice.Kind {
		case "teardown_blocked":
			blocked++
			if !strings.Contains(notice.Summary, "PID "+strconv.Itoa(loopPID)+" ") {
				t.Errorf("teardown_blocked Notice does not name the residual shell %d: %s", loopPID, notice.Summary)
			}
			if transientChild.MatchString(notice.Summary) {
				t.Errorf("teardown_blocked Notice names a transient child: %s", notice.Summary)
			}
		case "teardown_incomplete":
			incomplete++
			if strings.Contains(notice.Summary, "appeared during teardown") {
				t.Errorf("a process that predates Teardown was reported as appearing during it: %s", notice.Summary)
			}
		}
	}
	if blocked != 1 {
		t.Fatalf("teardown_blocked Notices = %d after 6 retries, want 1: %#v", blocked, notices)
	}
	if incomplete > 1 {
		t.Fatalf("teardown_incomplete Notices = %d after 6 retries, want at most 1: %#v", incomplete, notices)
	}
}
