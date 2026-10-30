//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// runWatchPass runs one full pass of the Project Watch through `posse
// _watch-pass` and returns once that pass has finished.
func runWatchPass(t *testing.T, binary, cwd string, env []string) {
	t.Helper()
	runPosse(t, binary, cwd, env, "_watch-pass")
}

// waitForWatchPasses runs up to passes Watch passes and reports whether
// condition held after one of them. A pass that serves the request starts
// after it, so the bound counts Watch work instead of wall-clock time.
func waitForWatchPasses(t *testing.T, binary, cwd string, env []string, passes int, condition func() bool) bool {
	t.Helper()
	for range passes {
		if condition() {
			return true
		}
		runWatchPass(t, binary, cwd, env)
	}
	return condition()
}

// eventWaitMargin keeps time to report a failure and clean up the fixture
// before the test binary's own deadline.
const eventWaitMargin = time.Minute

// waitForEvent polls condition until it holds. It has no wall-clock budget of
// its own: it gives up when abandoned returns a reason the event can no longer
// happen, such as the process that would cause it having exited, or when the
// test binary's deadline is near.
func waitForEvent(t *testing.T, condition func() bool, abandoned func() string) (bool, string) {
	t.Helper()
	deadline, bounded := t.Deadline()
	for {
		if condition() {
			return true, ""
		}
		if abandoned != nil {
			if reason := abandoned(); reason != "" {
				return condition(), reason
			}
		}
		if bounded && time.Until(deadline) < eventWaitMargin {
			return condition(), "the test deadline is near"
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForTaskState waits until the Task reaches want, and gives up as soon as
// it settles in a state from which want can no longer follow.
func waitForTaskState(t *testing.T, db *store.DB, projectID int64, ref string, want store.State) (store.Task, bool, string) {
	t.Helper()
	var task store.Task
	ok, reason := waitForEvent(t, func() bool {
		current, err := db.Task(context.Background(), projectID, ref)
		if err != nil {
			return false
		}
		task = current
		return current.State == want
	}, func() string { return taskStateUnreachable(ref, task, want) })
	return task, ok, reason
}

// taskStateUnreachable names the state when task has settled where want can
// no longer follow, and returns "" otherwise.
func taskStateUnreachable(ref string, task store.Task, want store.State) string {
	if task.State != want && slices.Contains([]store.State{store.StateFailed, store.StateLost, store.StateStalled, store.StateTornDown}, task.State) {
		return fmt.Sprintf("Task %s reached %s", ref, task.State)
	}
	return ""
}
