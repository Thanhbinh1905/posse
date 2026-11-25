//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// staleGeneration is the generation the posse Project stayed held on from
// 2026-10-27 to 2026-10-28, while every pane already ran in a newer one.
const staleGeneration = "7367:2239"

// holdOnStaleGeneration leaves the Project where the failed 2026-10-28
// recovery left it: the Lead and Workers run in the current Herdr generation,
// but the recorded generation is still the old one.
func (f *workerTabsFixture) holdOnStaleGeneration(t *testing.T) {
	t.Helper()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetProjectServerStartedAt(context.Background(), f.projectID, staleGeneration); err != nil {
		t.Fatal(err)
	}
}

func (f *workerTabsFixture) recordedGeneration(t *testing.T) string {
	t.Helper()
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	generation, err := db.ProjectServerStartedAt(context.Background(), f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestWatchAdoptsGenerationWhenEveryPaneIsLive(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	f.start(t, "t1", "Adopted worker", "adopted")
	current := f.snapshot(t).ServerStartedAt
	if current == "" || current == staleGeneration {
		t.Fatalf("Herdr generation = %q", current)
	}
	f.holdOnStaleGeneration(t)
	worker := f.task(t, "t1")
	if _, err := f.client.Run(context.Background(), "pane", "report-agent", worker.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", "idle"); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET agent_server_started_at=?,agent_status='working' WHERE id=?`, staleGeneration, worker.ID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	messageID, err := db.QueueMessage(context.Background(), worker.ID, "Continue after restart", false)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runPosse(t, f.binary, f.repo, f.env, "list")
	if !waitForCondition(20*time.Second, func() bool {
		if f.recordedGeneration(t) != current {
			return false
		}
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return false
		}
		defer db.Close()
		message, err := db.MessageByID(context.Background(), messageID)
		return err == nil && message.Status == "delivered"
	}) {
		t.Fatalf("Watch did not adopt generation %q and deliver the queued instruction", current)
	}
	notices, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	workerNotices, err := notices.Notices(context.Background(), f.projectID, false)
	_ = notices.Close()
	if err != nil {
		t.Fatal(err)
	}
	interrupted := 0
	for _, notice := range workerNotices {
		if notice.TaskID == worker.ID && notice.Kind == "worker_interrupted" {
			interrupted++
		}
	}
	if interrupted != 1 {
		t.Fatalf("worker_interrupted Notices = %d, want one", interrupted)
	}
	lead, found := findLeadInSnapshot(f.snapshot(t))
	if !found || lead.PaneID != f.leadPaneID {
		t.Fatalf("adoption moved or stopped the Lead: %#v", f.snapshot(t).Panes)
	}
}

func TestUpAllKeepsLiveLeadAndAdoptsGeneration(t *testing.T) {
	f := newWorkerTabsFixtureWithManualWatchOwner(t)
	f.start(t, "t1", "Adopted worker", "adopted")
	current := f.snapshot(t).ServerStartedAt
	f.holdOnStaleGeneration(t)
	// A restarted Lead never reports ready while this file exists, so a
	// restart fails the way the 2026-10-28 runs did.
	if err := os.WriteFile(filepath.Join(f.root, "hold-lead-start"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	output := runPosse(t, f.binary, f.repo, f.env, "up", "--all")
	if !strings.Contains(output, "state: restored") || strings.Contains(output, "failed_projects[1]") {
		t.Fatalf("up --all from a User shell failed the Project with a live Lead: %s", output)
	}
	if generation := f.recordedGeneration(t); generation != current {
		t.Fatalf("up --all left the generation at %q, want %q", generation, current)
	}
	lead, found := findLeadInSnapshot(f.snapshot(t))
	if !found || lead.PaneID != f.leadPaneID || lead.AgentStatus != "idle" {
		t.Fatalf("up --all did not keep the live Lead: %#v", f.snapshot(t).Panes)
	}
	if task := f.task(t, "t1"); task.State != store.StateWorking || task.Launches != 1 {
		t.Fatalf("up --all relaunched live Worker t1: %#v", task)
	}
}
