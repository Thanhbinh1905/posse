//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
)

func TestProjectCommandDoesNotWaitForeverForStoppedLockOwner(t *testing.T) {
	for _, kind := range []string{"mount", "fetch"} {
		t.Run(kind, func(t *testing.T) {
			f := newPRLifecycleFixtureWithLead(t, true)
			defer f.db.Close()
			path := f.db.Path + ".mount-lock"
			if kind == "fetch" {
				path = filepath.Join(f.repo, ".git", "posse-fetch.lock")
				configPath := filepath.Join(f.home, "config.toml")
				configData, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				configText := strings.Replace(string(configData), `pr_poll = "1ms"`, `pr_poll = "1h"`, 1)
				if configText == string(configData) {
					t.Fatal("fixture pr_poll setting not found")
				}
				if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := f.db.RecordCheckoutAttempt(context.Background(), f.project.ID, time.Now().UnixMilli()); err != nil {
					t.Fatalf("defer background checkout sync: %v", err)
				}
				// Let any sync already in progress finish before holding only the
				// fetch lock. The fresh checkout timestamp keeps the Lookout idle.
				syncLock := filepath.Join(f.repo, ".git", "posse-sync.lock")
				deadline := time.Now().Add(5 * time.Second)
				for {
					if err := exec.Command("flock", "-n", syncLock, "true").Run(); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("repository sync lock did not become available")
					}
					time.Sleep(25 * time.Millisecond)
				}
			}
			// STOP only after acquisition, removing the original test's timing
			// window. The child retains the same kernel lock as a paused Lookout.
			owner := exec.Command("sh", "-c", `exec 9>"$1"; flock -x 9; printf 'locked\n'; exec sleep 60`, "lock-owner", path)
			owner.Env = f.env
			ready, err := owner.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = syscall.Kill(owner.Process.Pid, syscall.SIGCONT)
				_ = owner.Process.Kill()
				_ = owner.Wait()
			}()
			locked := make(chan bool, 1)
			go func() { locked <- bufio.NewScanner(ready).Scan() }()
			select {
			case ok := <-locked:
				if !ok {
					t.Fatal("owner did not acquire lock")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lock setup timed out")
			}
			if err := syscall.Kill(owner.Process.Pid, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			wakeArgs := []string{"--json"}
			if kind == "fetch" {
				// Force a user-invoked Project sync through the fetch lock, while
				// background watch polling is deferred by the fresh timestamp.
				wakeArgs = append(wakeArgs, "sync")
			}
			wake := exec.CommandContext(ctx, f.binary, wakeArgs...)
			wake.WaitDelay = time.Second
			wake.Dir, wake.Env = f.repo, f.leadEnv
			started := time.Now()
			output, err := wake.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("CLI wake waited indefinitely for stopped lock owner: %s", time.Since(started))
			}
			var failure axi.Error
			if err == nil || json.Unmarshal(output, &failure) != nil || failure.Code != "store_busy" || !failure.Retryable {
				t.Fatalf("stopped owner failure = %s, %v; want retryable store_busy", output, err)
			}
			if kind == "fetch" {
				notices, err := f.db.Notices(context.Background(), f.project.ID, false)
				if err != nil {
					t.Fatal(err)
				}
				for _, notice := range notices {
					if strings.Contains(notice.Summary, "posse-fetch.lock") {
						t.Fatalf("expected transient fetch contention created a Notice: %#v", notice)
					}
				}
			}
			// A user command can first wait for the sync lock held by the
			// Lookout, then for the fetch lock; both waits are independently
			// bounded at three seconds. Keep margin below the 8-second watchdog.
			if elapsed := time.Since(started); elapsed > 7*time.Second {
				t.Fatalf("lock wait exceeded bound: %s", elapsed)
			}
			if err := owner.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := owner.Wait(); err == nil {
				t.Fatal("expected killed owner")
			}
			// Kernel release on exit, not deleting the file, enables retry.
			if kind == "fetch" {
				runPosse(t, f.binary, f.repo, f.leadEnv, "sync")
			} else {
				runPosse(t, f.binary, f.repo, f.leadEnv)
			}
		})
	}
}
