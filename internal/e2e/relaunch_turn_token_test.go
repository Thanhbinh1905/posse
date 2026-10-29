//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Reproduce #737: relaunch.md did not name the new turn token, so a relaunched
// Worker reused launch:1 from the original launch.md and its first done Signal
// was refused with stale_worker_turn.
func TestRelaunchedWorkerFirstDoneSignalUsesRelaunchTurnToken(t *testing.T) {
	f := newWorkerTabsFixture(t)
	agentPath := filepath.Join(f.root, "bin", "claude")
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	worker := `case "$PWD/" in "$POSSE_E2E_WORKTREES/"*)
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
  IFS= read -r prompt || exit 0
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
  case "$prompt" in
  *relaunch.md*)
    file=${prompt#Read }
    file=${file%% and follow*}
    token=$(sed -n 's/^Current Worker turn token: ` + "`" + `\([^` + "`" + `]*\)` + "`" + `\..*/\1/p' "$file")
    rm -f "$PWD/.posse-e2e-agent-exit"
    git -c user.name=w -c user.email=w@example.com commit --allow-empty -m "relaunched work" >/dev/null
    "` + f.binary + `" signal done "Relaunched work" --turn "$token" > "$POSSE_TEST_ROOT/relaunch-done.log" 2>&1 || true
    ;;
  esac
  while [ ! -f "$PWD/.posse-e2e-agent-exit" ]; do sleep 0.05; done
  exit 0
  ;;
esac
`
	if err := os.WriteFile(agentPath, []byte(strings.Replace(string(agent), "#!/bin/sh\n", "#!/bin/sh\n"+worker, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	f.start(t, "t1", "Relaunch turn token", "relaunch-turn")
	f.exitWorker(t, "t1")
	runPosse(t, f.binary, f.repo, f.leadEnv, "relaunch", "t1")
	if !waitForCondition(30*time.Second, func() bool { return f.task(t, "t1").State == store.StateDone }) {
		log, _ := os.ReadFile(filepath.Join(f.root, "relaunch-done.log"))
		t.Fatalf("relaunched Worker did not reach done: %s", log)
	}

	log, _ := os.ReadFile(filepath.Join(f.root, "relaunch-done.log"))
	if strings.Contains(string(log), "stale_worker_turn") {
		t.Fatalf("relaunched Worker's first done Signal was refused: %s", log)
	}
}
