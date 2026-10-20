//go:build e2e

package e2e

import (
	"bytes"
	"context"
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

func TestSupervised014PollerReexecsThroughWatchCompatibility(t *testing.T) {
	f := newPRLifecycleFixtureWithManualWatchOwner(t)
	defer f.db.Close()

	oldBinary := filepath.Join(f.root, "bin", "posse-legacy")
	newBinary := filepath.Join(f.root, "bin", "posse-current")
	binary, err := os.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldBinary, newBinary} {
		if err := os.WriteFile(path, binary, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldInfo, err := os.Stat(oldBinary)
	if err != nil {
		t.Fatal(err)
	}
	oldStat, ok := oldInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("cannot inspect legacy executable identity")
	}
	newInfo, err := os.Stat(newBinary)
	if err != nil {
		t.Fatal(err)
	}
	newStat, ok := newInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("cannot inspect current executable identity")
	}

	const token = "e2e-legacy-poller-token"
	startedAt := time.Now().Unix()
	gate := filepath.Join(f.root, "start-legacy-poller")
	launcher := filepath.Join(f.root, "bin", "legacy-poller-launcher")
	script := "#!/bin/sh\nwhile [ ! -e " + shellQuote(gate) + " ]; do sleep 0.01; done\nexec " + shellQuote(oldBinary) + " lookout --poll-only\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	setup := fmt.Sprintf("{\n  \"version\": \"0.15.0\",\n  \"binary\": %q\n}\n", newBinary)
	if err := os.WriteFile(filepath.Join(f.home, "setup.json"), []byte(setup), 0o600); err != nil {
		t.Fatal(err)
	}

	output := &bytes.Buffer{}
	command := exec.Command(launcher)
	command.Dir = f.repo
	command.Env = legacyPollerEnvironment(f.env, f.home, f.project.ID, token, startedAt)
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		_ = command.Wait()
	}()

	watch := store.WatchRuntime{
		PID: command.Process.Pid, Token: token, StartedAt: startedAt, HeartbeatAt: startedAt,
		ExecutablePath: oldBinary, ExecutableDevice: uint64(oldStat.Dev), ExecutableInode: oldStat.Ino,
	}
	if err := f.db.SetWatchRuntime(context.Background(), f.project.ID, watch); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		runtime, err := f.db.WatchRuntime(context.Background(), f.project.ID)
		if err == nil && runtime.PID == command.Process.Pid && runtime.Token == token && runtime.ExecutableDevice == uint64(newStat.Dev) && runtime.ExecutableInode == newStat.Ino {
			processInfo, statErr := os.Stat(fmt.Sprintf("/proc/%d/exe", command.Process.Pid))
			if statErr == nil {
				if processStat, ok := processInfo.Sys().(*syscall.Stat_t); ok && processStat.Ino == newStat.Ino && uint64(processStat.Dev) == uint64(newStat.Dev) {
					time.Sleep(250 * time.Millisecond)
					if err := command.Process.Signal(syscall.Signal(0)); err == nil {
						return
					}
				}
			}
		}
		if err := command.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("legacy poller exited before re-exec: %v\n%s", err, output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("legacy poller did not re-exec into the installed Watch; output: %s", output.String())
}

func legacyPollerEnvironment(base []string, home string, projectID int64, token string, startedAt int64) []string {
	filtered := make([]string, 0, len(base)+5)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "HERDR_") || strings.HasPrefix(key, "POSSE_WATCH_") || strings.HasPrefix(key, "POSSE_LOOKOUT_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	filtered = append(filtered,
		"POSSE_HOME="+home,
		fmt.Sprintf("POSSE_LOOKOUT_PROJECT_ID=%d", projectID),
		"POSSE_LOOKOUT_TOKEN="+token,
		fmt.Sprintf("POSSE_LOOKOUT_STARTED_AT=%d", startedAt),
	)
	return filtered
}
