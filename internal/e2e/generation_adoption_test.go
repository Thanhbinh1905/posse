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
	f := newWorkerTabsFixture(t)
	f.start(t, "t1", "Adopted worker", "adopted")
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("hidden watch not running: %v", watchPIDs(f.root))
	}
	current := f.snapshot(t).ServerStartedAt
	if current == "" || current == staleGeneration {
		t.Fatalf("Herdr generation = %q", current)
	}
	f.holdOnStaleGeneration(t)
	if !waitForCondition(20*time.Second, func() bool { return f.recordedGeneration(t) == current }) {
		t.Fatalf("watch kept the Project held on %q while every pane runs in %q", f.recordedGeneration(t), current)
	}
	lead, found := findLeadInSnapshot(f.snapshot(t))
	if !found || lead.PaneID != f.leadPaneID {
		t.Fatalf("adoption moved or stopped the Lead: %#v", f.snapshot(t).Panes)
	}
	if task := f.task(t, "t1"); task.State != store.StateWorking {
		t.Fatalf("adoption changed Worker t1: %#v", task)
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
