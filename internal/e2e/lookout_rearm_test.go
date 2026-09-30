//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestLookoutRecoveryBackoffSurvivesLeadRearm(t *testing.T) {
	f := newPRLifecycleFixtureWithSlowLoginShell(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatal("initial Lookout did not start")
	}
	before := lookoutPanes(t, client)
	if len(before) != 1 {
		t.Fatalf("initial Lookout tabs = %d, want 1", len(before))
	}
	for _, pid := range lookoutPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatal("initial Lookout did not stop")
	}
	stuckShell := filepath.Join(f.root, "bin", "slow-login-shell")
	if err := os.WriteFile(stuckShell, []byte("#!/bin/sh\nsleep 300\nexec /bin/sh -l\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := f.db.SetLookoutRecovery(ctx, f.project.ID, store.LookoutRecovery{
		PaneID: before[0].PaneID, RetryAt: time.Now().Add(-time.Second).UnixMilli(), FailedStarts: 3, NoticeRaised: true,
	}); err != nil {
		t.Fatal(err)
	}
	noticeID, err := f.db.CreateNotice(ctx, store.Notice{
		ProjectID: f.project.ID, Kind: "pr_watch_failing", Summary: "Lookout failed to start after repeated retries; recovery will continue with backoff", DataJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkNoticesDelivered(ctx, f.project.ID, []int64{noticeID}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(f.binary, "lookout", "--ack", "all", "--timeout", "500")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("re-armed Lead lookout: %v\n%s", err, output)
	}
	panes := lookoutPanes(t, client)
	if len(panes) != 1 || panes[0].TabID == before[0].TabID {
		t.Fatalf("persisted failed-start retry did not replace exactly one tab: before=%v after=%v", before, panes)
	}
	state, err := f.db.LookoutRecovery(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.FailedStarts != 4 || !state.NoticeRaised {
		t.Fatalf("re-armed recovery state = %#v, want failedStarts=4 with Notice already raised", state)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, notice := range notices {
		if notice.Kind == "pr_watch_failing" && notice.Summary == "Lookout failed to start after repeated retries; recovery will continue with backoff" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("re-armed recovery created %d copies of the startup Notice, want exactly 1", count)
	}
}
