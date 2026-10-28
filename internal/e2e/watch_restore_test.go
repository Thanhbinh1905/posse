//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestWatchRepairRestoresHeldProjectWithoutExecutorRecovery(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("initial hidden watch not running: %v", watchPIDs(f.root))
	}
	if panes := watchPanes(t, client); len(panes) != 0 {
		t.Fatalf("Project has visible Watch panes: %#v", panes)
	}

	configPath := filepath.Join(f.home, "config.toml")
	originalConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// The recorded Lead pane did not come back, so the Project stays held
	// rather than adopting the current generation.
	if err := f.db.SetProjectLead(context.Background(), f.project.ID, f.project.HerdrWorkspaceID, "stale-lead-pane", "posse:shop:stale-lead"); err != nil {
		t.Fatal(err)
	}
	beforeProject, err := f.db.ProjectByID(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRecovery, err := f.db.ProjectRecovery(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectServerStartedAt(context.Background(), f.project.ID, "stale-herdr-generation"); err != nil {
		t.Fatal(err)
	}
	beforeGeneration, err := f.db.ProjectServerStartedAt(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, pid := range watchPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("killed watch remains visible: %v", watchPIDs(f.root))
	}

	// A recorded generation that differs from Herdr's holds the Project for
	// an explicit restore.
	recovery := runPosse(t, f.binary, f.repo, f.env, "_herdr_started")
	if !strings.Contains(recovery, "held_projects") || !strings.Contains(recovery, "shop") {
		t.Fatalf("the startup hook did not hold the Project: %s", recovery)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("command supervision did not repair the held Project watch: %v", watchPIDs(f.root))
	}
	project, err := f.db.ProjectByID(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if project.LeadPaneID != beforeProject.LeadPaneID || project.HerdrWorkspaceID != beforeProject.HerdrWorkspaceID || project.Status != beforeProject.Status {
		t.Fatalf("watch repair changed the held Lead: before=%#v after=%#v", beforeProject, project)
	}
	afterRecovery, err := f.db.ProjectRecovery(context.Background(), f.project.ID)
	if err != nil || afterRecovery != beforeRecovery {
		t.Fatalf("watch repair changed executor recovery: before=%#v after=%#v err=%v", beforeRecovery, afterRecovery, err)
	}
	afterGeneration, err := f.db.ProjectServerStartedAt(context.Background(), f.project.ID)
	if err != nil || afterGeneration != beforeGeneration {
		t.Fatalf("watch repair changed Herdr generation: %q -> %q (%v)", beforeGeneration, afterGeneration, err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || string(configAfter) != string(originalConfig) {
		t.Fatalf("watch repair changed config: %v", err)
	}
	if panes := watchPanes(t, client); len(panes) != 0 {
		t.Fatalf("repair opened visible Watch panes: %#v", panes)
	}

	for _, pid := range watchPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(watchPIDs(f.root)) == 0 }) {
		t.Fatalf("watch survived SIGKILL before explicit repair: %v", watchPIDs(f.root))
	}
	explicitRepair := runPosse(t, f.binary, f.repo, f.env, "watch", "--repair")
	if !strings.Contains(explicitRepair, "repaired: true") || !waitForCondition(10*time.Second, func() bool { return len(watchPIDs(f.root)) == 1 }) {
		t.Fatalf("explicit repair did not restore the held Project watch: %s; pids=%v", explicitRepair, watchPIDs(f.root))
	}
	if panes := watchPanes(t, client); len(panes) != 0 {
		t.Fatalf("explicit repair opened visible Watch panes: %#v", panes)
	}

	output := runPosse(t, f.binary, f.repo, f.env, "watch", "--repair")
	if !strings.Contains(output, "state: running") || !strings.Contains(output, "repaired: false") {
		t.Fatalf("idempotent repair did not report the running watch: %s", output)
	}
	if pids := watchPIDs(f.root); len(pids) != 1 {
		t.Fatalf("repair left %d watch processes: %v", len(pids), pids)
	}

	down := runPosse(t, f.binary, f.repo, f.env, "down")
	if !strings.Contains(down, "state: down") {
		t.Fatalf("down did not report the Project stopped: %s", down)
	}
	runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if pids := watchPIDs(f.root); len(pids) != 0 {
		t.Fatalf("a later Project command restarted the down Project watch: %v", pids)
	}
	project, err = f.db.ProjectByID(context.Background(), f.project.ID)
	if err != nil || !project.IsDown() {
		t.Fatalf("Project left down state: %#v %v", project, err)
	}
	if _, err := f.db.WatchRuntime(context.Background(), f.project.ID); err != nil {
		t.Fatal(err)
	} else if state, _ := f.db.WatchRuntime(context.Background(), f.project.ID); state.PID != 0 {
		t.Fatalf("down retained watch state: %#v", state)
	}

}
