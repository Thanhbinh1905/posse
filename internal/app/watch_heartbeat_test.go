package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWatchSupervisionPreservesHeartbeatDuringSlowForgePoll(t *testing.T) {
	fixture := newPRMergeFixture(t, "pr", store.StateWorking)
	defer fixture.db.Close()
	ctx := context.Background()
	if err := fixture.db.UpdateTaskMerge(ctx, fixture.task.ID, "https://github.com/acme/shop/pull/17", ""); err != nil {
		t.Fatal(err)
	}

	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(key, "HERDR_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("POSSE_TEST_HERDR", "1")
	t.Setenv("POSSE_TEST_WATCH_RUNTIME_HEARTBEAT_INTERVAL", "20ms")
	t.Setenv("POSSE_TEST_WATCH_RUNTIME_STALE_AFTER", "100ms")
	t.Setenv("POSSE_TEST_EXTERNAL_COMMAND_TIMEOUT", "2s")
	started := filepath.Join(fixture.root, "slow-gh-started")
	finished := filepath.Join(fixture.root, "slow-gh-finished")
	t.Setenv("POSSE_TEST_SLOW_GH_STARTED", started)
	t.Setenv("POSSE_TEST_SLOW_GH_FINISHED", finished)
	realGH := filepath.Join(fixture.bin, "gh")
	wrappedGH := filepath.Join(fixture.bin, "gh-real")
	if err := os.Rename(realGH, wrappedGH); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nset -eu\nif [ \"$1 $2\" = \"api graphql\" ]; then touch \"$POSSE_TEST_SLOW_GH_STARTED\"; sleep 0.7; fi\n\"" + wrappedGH + "\" \"$@\"\nif [ \"$1 $2\" = \"api graphql\" ]; then touch \"$POSSE_TEST_SLOW_GH_FINISHED\"; fi\n"
	if err := os.WriteFile(realGH, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}

	oldInterval, oldStaleAfter := watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter
	watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter = oldInterval, oldStaleAfter })
	previousVerifier := watchExecutableVerifier
	watchExecutableVerifier = func(string) bool { return true }
	t.Cleanup(func() { watchExecutableVerifier = previousVerifier })
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previousStarter := startWatchProcess
	startWatchProcess = func(command *exec.Cmd) error {
		if filepath.Clean(command.Path) == filepath.Clean(testBinary) {
			return errors.New("test guard refused to start the Go test binary as a Watch")
		}
		return previousStarter(command)
	}
	t.Cleanup(func() { startWatchProcess = previousStarter })

	binary := filepath.Join(fixture.root, "bin", "posse")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(workingDir, "../.."))
	build := exec.Command("go", "build", "-o", binary, "./cmd/posse")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Posse CLI: %v\n%s", err, output)
	}

	if err := fixture.service.startWatchRuntime(ctx, fixture.db, fixture.home, fixture.project, binary); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.db.WatchRuntime(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var watchToStop watchProcess
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		watchToStop, _ = inspectWatchProcess(fixture.home, before.PID)
		if watchPIDRunning(before.PID) && watchToStop.PID == before.PID {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if watchToStop.PID != before.PID {
		t.Fatalf("Project Watch PID %d was not discoverable", before.PID)
	}
	t.Cleanup(func() {
		if err := stopStoredWatchRuntime(context.Background(), fixture.home, watchToStop); err != nil {
			t.Errorf("stop Project Watch PID %d: %v", before.PID, err)
		}
	})

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("fake forge call did not start: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(finished); err == nil {
		t.Fatal("fake forge call finished before supervision ran")
	}
	current, err := fixture.db.WatchRuntime(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.HeartbeatAt <= before.HeartbeatAt || time.Since(time.UnixMilli(current.HeartbeatAt)) > watchRuntimeStaleAfter {
		t.Fatalf("Project Watch heartbeat did not stay fresh during the blocked forge call: before=%d current=%d age=%s", before.HeartbeatAt, current.HeartbeatAt, time.Since(time.UnixMilli(current.HeartbeatAt)))
	}

	service := New(fixture.home, fixture.service.Herdr)
	output := &bytes.Buffer{}
	service.superviseWatchRuntimes(&axi.Context{Context: ctx, Out: output, ErrOut: output}, "", nil)
	after, err := fixture.db.WatchRuntime(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PID != before.PID || after.Token != before.Token {
		t.Fatalf("supervision replaced the healthy Project Watch during its forge call: before=%#v after=%#v output=%s", before, after, output)
	}
	notices, err := fixture.db.Notices(ctx, fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if countNoticeKind(notices, "pr_watch_failing") != 0 {
		t.Fatalf("supervision raised pr_watch_failing during a healthy slow poll: %#v", notices)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(finished); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fake forge call did not finish")
}
