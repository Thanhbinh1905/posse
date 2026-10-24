//go:build e2e && linux

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// TestStartFallsBackFromSandboxResetFailureWithOneLease verifies that a reset
// failure during `posse start` quarantines the chosen Sandbox without leaving
// it leased to the Task, and that the Notice explains the failure and repair.
func TestStartFallsBackFromSandboxResetFailureWithOneLease(t *testing.T) {
	f := newWorkerTabsFixture(t)
	ctx := context.Background()
	first := f.start(t, "t1", "Seed Sandbox", "seed-sandbox")
	markTaskReported(t, f, first)
	f.exitWorker(t, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1")

	// Fail checkout only inside sandbox-1 so the idle Sandbox cannot be reset
	// while a fresh Sandbox is still created normally.
	hooks := filepath.Join(f.repo, ".git", "hooks")
	script := "#!/bin/sh\ncase \"$(pwd)\" in */sandbox-1) echo 'injected post-checkout failure' >&2; exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	second := f.start(t, "t2", "Fall back", "fall-back")
	if strings.HasSuffix(second.WorktreePath, "sandbox-1") {
		t.Fatalf("Task t2 reused the Sandbox that cannot be reset: %s", second.WorktreePath)
	}

	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sandboxes, err := db.Sandboxes(ctx, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	broken := store.Sandbox{}
	for _, sandbox := range sandboxes {
		if sandbox.Number == 1 {
			broken = sandbox
		}
	}
	if broken.State != "broken" || broken.TaskID != 0 {
		t.Fatalf("sandbox-1 was not quarantined: %#v", sandboxes)
	}
	leases, err := db.AllResourceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var taskLeases []string
	for _, lease := range leases {
		if lease.ResourceType == "path" && lease.OwnerTaskID == second.ID {
			taskLeases = append(taskLeases, lease.Owner)
		}
		if lease.SandboxID == broken.ID {
			t.Fatalf("broken Sandbox still holds lease %d (%s)", lease.ID, lease.Owner)
		}
	}
	if len(taskLeases) != 1 || !strings.Contains(taskLeases[0], "sandbox-2") {
		t.Fatalf("Task t2 must hold exactly the sandbox-2 lease: %v", taskLeases)
	}

	var summary string
	if err := db.QueryRowContext(ctx, `SELECT summary FROM notices WHERE project_id=? AND kind='sandbox_broken'`, f.projectID).Scan(&summary); err != nil {
		t.Fatalf("no sandbox_broken Notice raised: %v", err)
	}
	for _, want := range []string{"sandbox-1", "injected post-checkout failure", "posse pool repair sandbox-1"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("sandbox_broken Notice lacks %q: %s", want, summary)
		}
	}
}
