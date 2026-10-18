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

func TestLookoutRepairRestoresHeldProjectWithoutExecutorRecovery(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("initial hidden poller not running: %v", lookoutPIDs(f.root))
	}
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("Project has visible Lookout panes: %#v", panes)
	}

	configPath := filepath.Join(f.home, "config.toml")
	originalConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	heldConfig := strings.Replace(string(originalConfig), "auto_unsaddle = \"finished\"", "auto_unsaddle = \"finished\"\nauto_recover = false", 1)
	if heldConfig == string(originalConfig) {
		t.Fatal("fixture config has no defaults.auto_unsaddle line")
	}
	if err := os.WriteFile(configPath, []byte(heldConfig), 0o600); err != nil {
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

	for _, pid := range lookoutPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatalf("killed poller remains visible: %v", lookoutPIDs(f.root))
	}

	recovery := runPosse(t, f.binary, f.repo, f.env, "recover", "--all")
	if !strings.Contains(recovery, "held_projects") || !strings.Contains(recovery, "shop") {
		t.Fatalf("recover --all did not leave the held Project alone: %s", recovery)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("command supervision did not repair the held Project poller: %v", lookoutPIDs(f.root))
	}
	project, err := f.db.ProjectByID(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if project.LeadPaneID != beforeProject.LeadPaneID || project.HerdrWorkspaceID != beforeProject.HerdrWorkspaceID || project.Status != beforeProject.Status {
		t.Fatalf("poller repair changed the held Lead: before=%#v after=%#v", beforeProject, project)
	}
	afterRecovery, err := f.db.ProjectRecovery(context.Background(), f.project.ID)
	if err != nil || afterRecovery != beforeRecovery {
		t.Fatalf("poller repair changed executor recovery: before=%#v after=%#v err=%v", beforeRecovery, afterRecovery, err)
	}
	afterGeneration, err := f.db.ProjectServerStartedAt(context.Background(), f.project.ID)
	if err != nil || afterGeneration != beforeGeneration {
		t.Fatalf("poller repair changed Herdr generation: %q -> %q (%v)", beforeGeneration, afterGeneration, err)
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || string(configAfter) != heldConfig {
		t.Fatalf("poller repair changed config: %v", err)
	}
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("repair opened visible Lookout panes: %#v", panes)
	}

	for _, pid := range lookoutPIDs(f.root) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	if !waitForCondition(5*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatalf("poller survived SIGKILL before explicit repair: %v", lookoutPIDs(f.root))
	}
	explicitRepair := runPosse(t, f.binary, f.repo, f.env, "lookout", "--repair")
	if !strings.Contains(explicitRepair, "repaired: true") || !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("explicit repair did not restore the held Project poller: %s; pids=%v", explicitRepair, lookoutPIDs(f.root))
	}
	if panes := lookoutPanes(t, client); len(panes) != 0 {
		t.Fatalf("explicit repair opened visible Lookout panes: %#v", panes)
	}

	output := runPosse(t, f.binary, f.repo, f.env, "lookout", "--repair")
	if !strings.Contains(output, "state: running") || !strings.Contains(output, "repaired: false") {
		t.Fatalf("idempotent repair did not report the running poller: %s", output)
	}
	if pids := lookoutPIDs(f.root); len(pids) != 1 {
		t.Fatalf("repair left %d poller processes: %v", len(pids), pids)
	}

	down := runPosse(t, f.binary, f.repo, f.env, "down")
	if !strings.Contains(down, "state: down") {
		t.Fatalf("down did not report the Project stopped: %s", down)
	}
	runPosse(t, f.binary, f.repo, f.env, "doctor", "--json")
	if pids := lookoutPIDs(f.root); len(pids) != 0 {
		t.Fatalf("a later Project command restarted the down Project poller: %v", pids)
	}
	project, err = f.db.ProjectByID(context.Background(), f.project.ID)
	if err != nil || !project.IsDown() {
		t.Fatalf("Project left down state: %#v %v", project, err)
	}
	if _, err := f.db.LookoutPoller(context.Background(), f.project.ID); err != nil {
		t.Fatal(err)
	} else if state, _ := f.db.LookoutPoller(context.Background(), f.project.ID); state.PID != 0 {
		t.Fatalf("down retained poller state: %#v", state)
	}

}
