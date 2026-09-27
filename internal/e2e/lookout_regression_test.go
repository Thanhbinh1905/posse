//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func lookoutPanes(t *testing.T, client *herdr.Client) []herdr.Pane {
	t.Helper()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var panes []herdr.Pane
	for _, pane := range snapshot.Panes {
		if pane.Label == "posse:shop:lookout" {
			panes = append(panes, pane)
		}
	}
	return panes
}

// lookoutPIDs lists poll-only Lookout processes that belong to this fixture.
func lookoutPIDs(root string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if !strings.Contains(string(cmdline), "--poll-only") {
			continue
		}
		environ, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if strings.Contains(string(environ), root) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// A dead Lookout leaves a labeled tab while Herdr and the Lead stay up.
// The Lead's ordinary lookout tick must restart it without `posse up`.
func TestLookoutRestartsAfterProcessExit(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Fatalf("Lookout process not running: %v", lookoutPIDs(f.root))
	}
	for _, pid := range lookoutPIDs(f.root) {
		_ = exec.Command("kill", "-INT", strconv.Itoa(pid)).Run()
	}
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 0 }) {
		t.Fatal("Lookout did not exit")
	}
	panes := lookoutPanes(t, client)
	t.Logf("Lookout panes after exit: %d", len(panes))
	command := exec.Command(f.binary, "lookout", "--ack", "all", "--timeout", "3000")
	command.Dir, command.Env = f.repo, f.leadEnv
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Lead lookout: %v %s", err, output)
	}
	if !waitForCondition(10*time.Second, func() bool { return len(lookoutPIDs(f.root)) == 1 }) {
		t.Errorf("no Lookout process after Lead tick; panes labelled Lookout=%d", len(lookoutPanes(t, client)))
	}
}

// L2: Herdr restarts. Recovery restarts the Lead but must also restore the
// Lookout tab, or nothing polls while the Lead is idle.
func TestLookoutRestartsAfterHerdrRestart(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	client := herdr.NewWithEnv("herdr", f.env)
	if len(lookoutPanes(t, client)) != 1 {
		t.Fatal("Lookout tab missing before restart")
	}
	if err := client.StopIsolatedServer(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	startServer(t, client)
	env := f.env
	for _, key := range []string{"HERDR_PANE_ID", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID"} {
		env = setEnv(env, key, "")
	}
	command := exec.Command(f.binary, "recover", "--all")
	command.Dir, command.Env = f.repo, env
	output, err := command.CombinedOutput()
	t.Logf("recover --all: %v %s", err, output)
	project, err := f.db.ProjectByName(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := client.Snapshot(context.Background())
	leadFound := false
	for _, pane := range snapshot.Panes {
		if pane.PaneID == project.LeadPaneID {
			leadFound = true
		}
	}
	t.Logf("Lead pane restored=%v workspace=%s", leadFound, project.HerdrWorkspaceID)
	if !waitForCondition(15*time.Second, func() bool {
		return len(lookoutPanes(t, client)) > 0 && len(lookoutPIDs(f.root)) > 0
	}) {
		t.Errorf("Lookout not restored after Herdr restart: panes=%d processes=%d", len(lookoutPanes(t, client)), len(lookoutPIDs(f.root)))
	}
	_ = store.StateLanded
}

// L3: the Lookout tab and ordinary Lead commands both run automatic Teardown.
// A merged PR must be torn down without a false "teardown incomplete" reason.
func TestConcurrentLookoutTeardownRaisesNoFalseReason(t *testing.T) {
	f := newPRLifecycleFixture(t)
	defer f.db.Close()
	brief := filepath.Join(f.root, "lookout-follow-up.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: PR follow-up\ndone_when: committed change exists\n---\nMake a PR change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.rideAndComplete(t, brief, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "land", "t1")
	landing := f.mustTask(t, "t1")
	merge := f.mergeOnLocalOrigin(t, landing, "t1")
	f.writeGraphQL(t, "pr1", "MERGED", "SUCCESS", "APPROVED", "MERGEABLE", merge, landing.GatedSHA)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		command := exec.Command(f.binary, "show", "t1")
		command.Dir, command.Env = f.repo, f.leadEnv
		_, _ = command.CombinedOutput()
		if f.mustTask(t, "t1").State == store.StateTornDown {
			break
		}
	}
	notices, err := f.db.Notices(context.Background(), f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notices {
		if n.Kind == "unsaddle_incomplete" {
			t.Errorf("state=%s false reason: %s", f.mustTask(t, "t1").State, n.Summary)
		}
	}
}
