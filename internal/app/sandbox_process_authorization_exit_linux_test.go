//go:build linux

package app

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSandboxAuthorizationTreatsZombieAsNotSignallable(t *testing.T) {
	child := exec.Command("sh", "-c", "sleep 1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	handle, err := openSandboxProcessHandle(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		zombie, err := sandboxProcessIsZombie(child.Process.Pid)
		if err == nil && zombie {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	zombie, err := sandboxProcessIsZombie(child.Process.Pid)
	if err != nil || !zombie {
		t.Fatalf("child state = zombie:%t err:%v, want an unreaped zombie", zombie, err)
	}
	alive, err := verifySandboxProcessHandleIfAlive(child.Process.Pid, handle)
	if err != nil || alive {
		t.Fatalf("unreaped zombie = alive:%t err:%v, want not signallable", alive, err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("wait for exited child: %v", err)
	}
	waited = true
}

func TestSandboxAuthorizationRechecksUnretainedExitedProcess(t *testing.T) {
	// A child can exit after CWD enumeration but before its handle is retained.
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	authorization := newSandboxProcessAuthorization()
	defer authorization.Close()
	if alive, err := authorization.verifyIfAlive(child.Process.Pid); alive || err != nil {
		t.Fatalf("exited unretained child = alive:%v err:%v, want proven exited", alive, err)
	}

	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = live.Process.Kill()
		_ = live.Wait()
	})
	if _, err := authorization.verifyIfAlive(live.Process.Pid); err == nil || !strings.Contains(err.Error(), "unverified process") {
		t.Fatalf("live unretained child = %v, want fail-closed refusal", err)
	}
	if err := syscall.Kill(live.Process.Pid, 0); err != nil {
		t.Fatalf("unretained child was signaled: %v", err)
	}
	if authorization.handle(live.Process.Pid) != nil {
		t.Fatal("liveness observation granted cleanup authority")
	}
}
