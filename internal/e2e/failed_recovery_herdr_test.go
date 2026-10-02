//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Real Herdr starts a real fake-harness process for each attempt. The harness
// remains idle after every prompt, exercising Herdr's actual prompt-wait error.
func TestFailedRecoveryBoundWithRealHerdr(t *testing.T) {
	f := newRiderTabsFixture(t)
	agentPath := filepath.Join(f.root, "bin", "claude")
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	startsPath := filepath.Join(f.root, "recovery-starts.log")
	script := strings.Replace(string(agent), "#!/bin/sh\n", "#!/bin/sh\ncase \"$PWD/\" in \"$POSSE_E2E_WORKTREES/\"*) printf 'start\\n' >> \"$POSSE_TEST_ROOT/recovery-starts.log\";; esac\n", 1)
	script = strings.Replace(script, "    IFS= read -r prompt || exit 0\n", "    IFS= read -r prompt || exit 0\n    if [ -f \"$POSSE_TEST_ROOT/fail-recovery-prompts\" ]; then\n      while IFS= read -r line; do :; done\n      exit 0\n    fi\n", 1)
	if err := os.WriteFile(agentPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	before := f.ride(t, "t1", "Always failing recovery", "always-failing-recovery")
	for _, setting := range [][]string{{"defaults.recovery_attempts", "2"}, {"defaults.recovery_backoff", "10ms"}} {
		runPosse(t, f.binary, f.repo, f.leadEnv, "config", "set", setting[0], setting[1], "--project", "shop")
	}
	if err := os.WriteFile(filepath.Join(f.root, "fail-recovery-prompts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	// A stale persisted generation reproduces the incident's mismatch without
	// starting any unrelated server or depending on Herdr's restore timing.
	err = db.SetProjectServerStartedAt(context.Background(), f.projectID, "before-restart")
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	startsBefore, err := os.ReadFile(startsPath)
	if err != nil {
		t.Fatal(err)
	}
	eventEnv := setEnv(f.env, "HERDR_PLUGIN_EVENT_JSON", fmt.Sprintf(`{"event":"pane.agent_status_changed","data":{"pane_id":%q,"agent_status":"working"}}`, f.leadPaneID))
	runPosse(t, f.binary, f.repo, eventEnv, "_ingest")
	for _, args := range [][]string{{"show", "t1"}, {"ack", "all"}, {"lead"}, {"lookout", "--timeout", "10"}} {
		runPosse(t, f.binary, f.repo, f.leadEnv, args...)
	}
	for i := 0; i < 8; i++ {
		runPosse(t, f.binary, f.repo, eventEnv, "_ingest")
	}
	after := f.task(t, "t1")
	startsAfter, err := os.ReadFile(startsPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err = store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := db.TaskRecovery(context.Background(), after.ID)
	if err != nil {
		t.Fatal(err)
	}
	var notices int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=?`, after.ID).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	starts := strings.Count(string(startsAfter), "start\n") - strings.Count(string(startsBefore), "start\n")
	if after.Launches != before.Launches+2 || starts != 2 || state.Attempts != 2 || state.Status != "exhausted" || notices != 1 || !strings.Contains(state.LastError, "prompt") {
		t.Fatalf("real Herdr failed recovery: launches %d -> %d, process starts=%d state=%#v Notices=%d", before.Launches, after.Launches, starts, state, notices)
	}
	t.Logf("real Herdr: configured bound=2, launches=%d, Rider process starts=%d, exhaustion Notices=%d; last error=%s", after.Launches-before.Launches, starts, notices, state.LastError)
}
