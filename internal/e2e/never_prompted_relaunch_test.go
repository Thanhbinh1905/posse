//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// Claude Code reports its session ID when it starts but persists the session
// only after its first prompt. A start that never delivered the Brief leaves a
// recorded session that `claude --resume` cannot find, so the recovery
// relaunch must start a fresh session and hand it the Brief.
func TestRelaunchAfterUndeliveredBriefStartsFreshSession(t *testing.T) {
	f := newWorkerTabsFixture(t)
	agentPath := filepath.Join(f.root, "bin", "claude")
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	worker := `case "$PWD/" in "$POSSE_E2E_WORKTREES/"*)
  printf '%s\n' "$*" >> "$POSSE_TEST_ROOT/worker-args.log"
  resume=""
  previous=""
  for arg in "$@"; do
    if [ "$previous" = "--resume" ]; then resume="$arg"; fi
    previous="$arg"
  done
  if [ -n "$resume" ] && ! grep -qx "$resume" "$POSSE_TEST_ROOT/persisted-sessions" 2>/dev/null; then
    echo "No conversation found with session ID: $resume"
    exit 1
  fi
  session="session-$$"
  printf '%s\n' "$session" >> "$POSSE_TEST_ROOT/started-sessions"
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
  IFS= read -r prompt || exit 0
  if [ -f "$POSSE_TEST_ROOT/drop-prompts" ]; then
    while IFS= read -r line; do :; done
    exit 0
  fi
  printf '%s\n' "$session" >> "$POSSE_TEST_ROOT/persisted-sessions"
  printf '%s\n' "$prompt" >> "$POSSE_TEST_ROOT/worker-prompts.log"
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
  while [ ! -f "$PWD/.posse-e2e-agent-exit" ]; do sleep 0.05; done
  exit 0
  ;;
esac
`
	if err := os.WriteFile(agentPath, []byte(strings.Replace(string(agent), "#!/bin/sh\n", "#!/bin/sh\n"+worker, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "drop-prompts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(f.root, "undelivered.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Undelivered Brief\ndone_when: the Worker receives its Brief\n---\nWait.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := runPosseAllowFailure(f, f.repo, f.leadEnv, "start", "--brief", brief, "--name", "undelivered-brief"); err == nil {
		t.Fatalf("start delivered a dropped prompt: %s", output)
	}
	f.reconcileUntilState(t, "t1", store.StateFailed)
	if err := os.Remove(filepath.Join(f.root, "drop-prompts")); err != nil {
		t.Fatal(err)
	}
	started, err := os.ReadFile(filepath.Join(f.root, "started-sessions"))
	if err != nil {
		t.Fatal(err)
	}
	session := strings.TrimSpace(string(started))

	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code's Herdr hook reports the session at startup and reconcile
	// records it, as it did for t585.
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET agent_session=?, agent_session_kind='claude' WHERE project_id=? AND seq=1`, `{"session_id":"`+session+`"}`, f.projectID); err != nil {
		t.Fatal(err)
	}
	decisions, err := db.Decisions(context.Background(), f.projectID, true)
	_ = db.Close()
	if err != nil || len(decisions) != 1 || decisions[0].Kind != "recovery" || !slices.Contains(decisions[0].Options, "relaunch") {
		t.Fatalf("want one recovery Decision offering relaunch, got %#v %v", decisions, err)
	}
	id := strconv.FormatInt(decisions[0].ID, 10)
	runPosse(t, f.binary, f.repo, f.leadEnv, "decide", id, "relaunch", "--user-approved", "945 relaunch")
	if output, err := runPosseAllowFailure(f, f.repo, f.leadEnv, "apply", id); err != nil {
		args, _ := os.ReadFile(filepath.Join(f.root, "worker-args.log"))
		t.Fatalf("apply relaunch: %v\n%s\nWorker launches:\n%s", err, output, args)
	}
	f.waitState(t, "t1", store.StateWorking)

	args, err := os.ReadFile(filepath.Join(f.root, "worker-args.log"))
	if err != nil {
		t.Fatal(err)
	}
	launches := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(launches) != 2 || strings.Contains(launches[1], "--resume") || strings.Contains(launches[1], session) {
		t.Fatalf("relaunch resumed a session that never received a prompt: %q", launches)
	}
	prompts, err := os.ReadFile(filepath.Join(f.root, "worker-prompts.log"))
	relaunchPath := filepath.Join(f.home, "projects", "shop", "tasks", "t1", "relaunch.md")
	if err != nil || !strings.Contains(string(prompts), relaunchPath) {
		t.Fatalf("fresh session did not receive the relaunch prompt: %q %v", prompts, err)
	}
	instructions, err := os.ReadFile(relaunchPath)
	if err != nil || !strings.Contains(string(instructions), "launch.md") {
		t.Fatalf("relaunch prompt does not lead to the Brief: %q %v", instructions, err)
	}
}

// An agent that exits before its Brief is delivered leaves a failed Task with
// no work. Teardown releases it without a User discard approval (#687).
func TestTeardownAfterAgentExitsBeforeBriefNeedsNoDiscard(t *testing.T) {
	f := newWorkerTabsFixture(t)
	agentPath := filepath.Join(f.root, "bin", "claude")
	agent, err := os.ReadFile(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	worker := `case "$PWD/" in "$POSSE_E2E_WORKTREES/"*)
  herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
  exit 0
  ;;
esac
`
	if err := os.WriteFile(agentPath, []byte(strings.Replace(string(agent), "#!/bin/sh\n", "#!/bin/sh\n"+worker, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(f.root, "exited.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Exited agent\ndone_when: the Worker receives its Brief\n---\nWait.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := runPosseAllowFailure(f, f.repo, f.leadEnv, "start", "--brief", brief, "--name", "exited-agent"); err == nil {
		t.Fatalf("start succeeded although the agent exited before its Brief: %s", output)
	}
	f.reconcileUntilState(t, "t1", store.StateFailed)
	runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1")
	f.waitState(t, "t1", store.StateTornDown)
}
