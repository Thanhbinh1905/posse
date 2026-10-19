//go:build e2e

package e2e

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestWatchRepairsDeadWatchesWithoutStaleNotices(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("initial hidden watch did not start: %v", watchPIDs(f.root))
	}
	if panes := watchPanes(t, client); len(panes) != 0 {
		t.Fatalf("watch created visible panes: %#v", panes)
	}

	for episode := 1; episode <= 2; episode++ {
		pids := watchPIDs(f.root)
		if len(pids) != 1 {
			t.Fatalf("episode %d began with watches %v", episode, pids)
		}
		if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
			t.Fatalf("episode %d watch survived SIGKILL: %v", episode, watchPIDs(f.root))
		}
		runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
		if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
			t.Fatalf("episode %d was not repaired: %v", episode, watchPIDs(f.root))
		}
		notices, err := f.db.Notices(context.Background(), f.project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, notice := range notices {
			if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Watch runtime") {
				count++
			}
		}
		if count != 0 {
			t.Fatalf("dead watch recovery after episode %d created a false stale-heartbeat Notice: %#v", episode, notices)
		}
	}
}
