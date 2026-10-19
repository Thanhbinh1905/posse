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

func TestLookoutRepairsDeadPollersWithoutStaleNotices(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("initial hidden poller did not start: %v", lookoutPIDs(f.root))
	}
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("poller created visible panes: %#v", panes)
	}

	for episode := 1; episode <= 2; episode++ {
		pids := lookoutPIDs(f.root)
		if len(pids) != 1 {
			t.Fatalf("episode %d began with pollers %v", episode, pids)
		}
		if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
			t.Fatalf("episode %d poller survived SIGKILL: %v", episode, lookoutPIDs(f.root))
		}
		runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
		if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
			t.Fatalf("episode %d was not repaired: %v", episode, lookoutPIDs(f.root))
		}
		notices, err := f.db.Notices(context.Background(), f.project.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, notice := range notices {
			if notice.Kind == "pr_watch_failing" && strings.Contains(notice.Summary, "Lookout poller") {
				count++
			}
		}
		if count != 0 {
			t.Fatalf("dead poller recovery after episode %d created a false stale-heartbeat Notice: %#v", episode, notices)
		}
	}
}
