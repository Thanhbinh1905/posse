package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestWatchHeartbeatStaysFreshDuringSlowForgeCall(t *testing.T) {
	fixture := newStartFixture(t)
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	oldInterval, oldStaleAfter := watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter
	watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter = 15*time.Millisecond, 75*time.Millisecond
	t.Cleanup(func() { watchRuntimeHeartbeatInterval, watchRuntimeStaleAfter = oldInterval, oldStaleAfter })

	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeForge := filepath.Join(fakeBin, "gh")
	if err := os.WriteFile(fakeForge, []byte("#!/bin/sh\nsleep 0.25\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	watch := store.WatchRuntime{PID: os.Getpid(), Token: "heartbeat-test", StartedAt: time.Now().UnixMilli(), HeartbeatAt: time.Now().UnixMilli()}
	if err := db.SetWatchRuntime(context.Background(), fixture.project.ID, watch); err != nil {
		t.Fatal(err)
	}
	stop := startWatchRuntimeHeartbeat(context.Background(), db, fixture.project.ID, watch, func(err error) { t.Errorf("heartbeat: %v", err) })
	defer stop()

	if _, err := commandOutputArgsWithTimeout(context.Background(), time.Second, "", "gh", "run", "view"); err != nil {
		t.Fatalf("slow fake forge call: %v", err)
	}
	current, err := db.WatchRuntime(context.Background(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	age := time.Since(time.UnixMilli(current.HeartbeatAt))
	if current.HeartbeatAt <= watch.HeartbeatAt || age > watchRuntimeStaleAfter {
		t.Fatalf("heartbeat did not stay fresh during slow forge call: heartbeat=%d age=%s", current.HeartbeatAt, age)
	}
}
