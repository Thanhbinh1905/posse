//go:build e2e && linux

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func TestGreenhouseSameHeadReviewerOverflowStopsAfterHistoricalLoop(t *testing.T) {
	f := newWorkerTabsFixture(t)
	projectConfig := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("review_provider = \"greenhouse\"\n[defaults]\nreview = \"full-pipeline\"\nmerge_mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse reviewer overflow", "greenhouse-overflow")
	head := c.authorCommit(t)
	c.hold(t)
	task := c.f.task(t, "t1")
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 858 {
		at := time.Now().Add(-time.Duration(858-attempt) * time.Minute).UnixMilli()
		if err := db.RecordGreenhouseReviewRequest(context.Background(), store.GreenhouseReviewRequest{
			ProjectID: task.ProjectID, TaskID: task.ID, Repository: "shop", RequestedHead: head, Provider: store.GreenhouseReviewProvider,
			RequestedAt: at, SettledAt: at + 1, Outcome: "failed",
		}); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.CreateGreenhouseNeedsDecisionNoticeOnce(context.Background(), task.ProjectID, task.ID, "shop", head, "Greenhouse review failed after 858 attempts. Needs a decision: split the change or land it another way. Latest failure: prompt exceeds maximum context length"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	workerEnv := setEnv(c.env, "POSSE_TASK_ID", c.change.ContainmentMarker)
	workerEnv = setEnv(workerEnv, "HERDR_PANE_ID", c.change.PaneID)
	if output, err := c.posse(workerEnv, c.change.WorktreePath, "signal", "done", "Request Greenhouse review", "--head", head); err != nil {
		t.Fatalf("Worker done Signal: %v\n%s", err, output)
	}
	if output, err := c.posse(c.env, f.repo, "watch", "--poll-only", "--timeout", "3000"); err != nil {
		t.Fatalf("Lead Watch: %v\n%s", err, output)
	}
	if got := c.invocations(t); got != 0 {
		t.Fatalf("reviewer ran %d times after 858 same-head failures", got)
	}
	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), task.ProjectID, false)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	failureNotices := 0
	for _, notice := range notices {
		if notice.TaskID == task.ID && notice.Kind == "needs_decision" {
			failureNotices++
			if !strings.Contains(notice.Summary, "maximum context length") {
				t.Errorf("failure Notice omitted latest reason: %s", notice.Summary)
			}
		}
	}
	if failureNotices != 1 {
		t.Fatalf("same-head needs-decision Notices = %d, want one", failureNotices)
	}
}

func TestGreenhouseParallelRetryExhaustionCountsOnceAfterRecovery(t *testing.T) {
	f := newWorkerTabsFixture(t)
	projectConfig := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte("review_parallel = \"greenhouse\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newGreenhouseContract(t, f)
	c.change = f.start(t, "t1", "Greenhouse retry exhaustion", "greenhouse-retry-exhaust")
	c.authorCommit(t)
	c.hold(t)

	binary := filepath.Join(f.root, "bin", "greenhouse")
	realBinary := binary + ".real"
	if err := os.Rename(binary, realBinary); err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf(`#!/bin/sh
if [ "$1" = axi ]; then
    request=$(cat)
    if printf '%%s' "$request" | grep -q '"op":"review"'; then
        printf '%%s' "$request" | '%s' axi >/dev/null 2>&1
        exit 1
    fi
    printf '%%s' "$request" | '%s' axi
    exit $?
fi
exec '%s' "$@"
`, realBinary, realBinary, realBinary)
	if err := os.WriteFile(binary, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := c.posse(c.env, f.repo, "_greenhouse", "review", "t1", "--reviewer-backend", "pi", "--reviewer-model", "fixture/model", "--timeout", "60000", "--json")
	if err == nil || !strings.Contains(output, "greenhouse_failed") {
		t.Fatalf("exhausted transport retry result: %v\n%s", err, output)
	}
	c.waitInvocations(t, 1)
	assertGreenhouseRequestStats(t, f, c.env, 1, 0)

	if err := os.Rename(realBinary, binary); err != nil {
		t.Fatal(err)
	}
	c.release(t)
	var recovered map[string]any
	if !waitForCondition(30*time.Second, func() bool {
		output, err := c.posse(c.env, f.repo, "_greenhouse", "review", "t1", "--reviewer-backend", "pi", "--reviewer-model", "fixture/model", "--timeout", "60000", "--json")
		if err != nil {
			return false
		}
		recovered = decodeGreenhouseOutput(t, output)
		return recovered["outcome"] == "settled"
	}) {
		t.Fatalf("retry-exhausted request did not recover: %v", recovered)
	}
	assertGreenhouseRequestStats(t, f, c.env, 1, 1)
}
