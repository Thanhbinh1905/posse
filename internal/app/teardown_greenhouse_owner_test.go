//go:build linux

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestTeardownStopsTaskGreenhouseOwnerAndClearsPendingRequest(t *testing.T) {
	f := newPRMergeFixture(t, "local", store.StateDone)
	roundDir := filepath.Join(f.home, "projects", f.project.Name, "tasks", taskIDString(f.task.Seq), "greenhouse")
	if err := os.MkdirAll(roundDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := parallelReviewLockPath(roundDir, f.project.Name)
	lock, busy, err := acquireParallelReviewLock(lockPath)
	if err != nil || busy {
		t.Fatalf("acquire owner lock: busy=%t err=%v", busy, err)
	}
	ready := filepath.Join(roundDir, "ready")
	child := exec.Command("python3", "-c", "import os,signal,time; open(os.environ['READY'], 'w').close(); time.sleep(30)", "_greenhouse", "review", taskIDString(f.task.Seq))
	child.Env = append(os.Environ(), "READY="+ready)
	child.ExtraFiles = []*os.File{lock}
	if err := child.Start(); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	_ = lock.Close()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("owner child did not start: %v", err)
	}
	if err := writeParallelReviewOwner(parallelReviewOwnerPath(roundDir, f.project.Name), parallelReviewOwner{PID: child.Process.Pid, Head: f.headSHA, RequestedAt: 1}); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(roundDir, f.project.Name+".pending.json")
	if err := os.WriteFile(pending, []byte(`{"grant":"shop/t1/shop/r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if child.Process.Pid <= 1 {
		t.Fatalf("unsafe child PID %d", child.Process.Pid)
	}
	if err := stopTaskGreenhouseReviewOwners(context.Background(), f.db, f.home, f.project, f.task); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("Greenhouse owner exited successfully after teardown signal")
	}
	waited = true
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("pending review request remains: %v", err)
	}
	if _, err := os.Stat(parallelReviewOwnerPath(roundDir, f.project.Name)); !os.IsNotExist(err) {
		t.Fatalf("review owner record remains: %v", err)
	}
	if inFlight, err := parallelReviewInFlight(lockPath); err != nil || inFlight {
		t.Fatalf("review lock remains in flight: %t, %v", inFlight, err)
	}
}
