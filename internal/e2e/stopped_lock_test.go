//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
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
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			wake := exec.CommandContext(ctx, f.binary, "--json")
			wake.WaitDelay = time.Second
			wake.Dir, wake.Env = f.repo, f.leadEnv
			started := time.Now()
			output, err := wake.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("CLI wake waited indefinitely for stopped lock owner: %s", time.Since(started))
			}
			if err != nil {
				var failure axi.Error
				if json.Unmarshal(output, &failure) != nil || failure.Code != "store_busy" || !failure.Retryable {
					t.Fatalf("stopped owner failure = %s, %v; want retryable store_busy or a recorded checkout-sync Notice", output, err)
				}
			} else {
				var response struct {
					Notices []struct {
						Kind    string `json:"kind"`
						Summary string `json:"summary"`
					} `json:"notices"`
				}
				if json.Unmarshal(output, &response) != nil {
					t.Fatalf("stopped owner response is not valid JSON: %s", output)
				}
				recordedContention := false
				for _, notice := range response.Notices {
					if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "checkout sync") && strings.Contains(notice.Summary, "posse-fetch.lock") {
						recordedContention = true
						break
					}
				}
				if !recordedContention {
					t.Fatalf("stopped owner response did not report checkout contention: %s", output)
				}
			}
			if elapsed := time.Since(started); elapsed > 8*time.Second {
				t.Fatalf("lock wait exceeded bound: %s", elapsed)
			}
			if err := owner.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := owner.Wait(); err == nil {
				t.Fatal("expected killed owner")
			}
			// Kernel release on exit, not deleting the file, enables retry.
			runPosse(t, f.binary, f.repo, f.leadEnv)
		})
	}
}
