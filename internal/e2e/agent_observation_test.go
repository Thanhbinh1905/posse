//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// posse, posse list and posse show label the observed Herdr agent status and
// its age beside the stored Task state, and flag a working Task whose agent
// has been idle past idle_after. The Task state never takes the observation.
func TestTaskViewsShowObservedAgentStatusBesideTaskState(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()
	ctx := context.Background()

	fakeAgent := `#!/bin/sh
set -eu
herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state idle >/dev/null 2>&1
case "$PWD/" in
  "$POSSE_E2E_WORKTREES/"*)
    IFS= read -r prompt || exit 0
    herdr pane report-agent "$HERDR_PANE_ID" --source posse.fake --agent claude --state working >/dev/null 2>&1
    ;;
esac
while IFS= read -r line; do :; done
`
	if err := os.WriteFile(filepath.Join(fixture.root, "bin", "claude"), []byte(fakeAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "research.md")
	if err := os.WriteFile(brief, []byte("---\ntype: research\ntitle: Observed agent\ndone_when: report.md is complete\n---\nWrite a concise Report.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "start", "--brief", brief, "--name", "observed-agent"); !strings.Contains(output, "t1") {
		t.Fatalf("start did not return t1: %s", output)
	}
	fixture.waitTaskState(t, "t1", store.StateWorking)
	task := fixture.mustTask(t, "t1")
	client := herdr.NewWithEnv("herdr", fixture.env)
	// waitStatus waits for Herdr to report exactly this status, so done and
	// idle are told apart.
	waitStatus := func(status string) {
		t.Helper()
		if !waitForCondition(10*time.Second, func() bool {
			snapshot, err := client.Snapshot(ctx)
			if err != nil {
				return false
			}
			for _, pane := range snapshot.Panes {
				if pane.PaneID == task.PaneID {
					return pane.AgentStatus == status
				}
			}
			return false
		}) {
			t.Fatalf("pane %s did not become %s", task.PaneID, status)
		}
	}
	report := func(status string) {
		t.Helper()
		if _, err := client.Run(ctx, "pane", "report-agent", task.PaneID, "--source", "posse.fake", "--agent", "claude", "--state", status); err != nil {
			t.Fatalf("report agent %s: %v", status, err)
		}
	}
	views := func() map[string]string {
		return map[string]string{
			"posse":           runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv),
			"posse --full":    runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "--full"),
			"posse list":      runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "list"),
			"posse show t1":   runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "show", "t1"),
			"posse list json": runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "list", "--json"),
		}
	}
	expectEvery := func(step string, outputs map[string]string, present []string, absent []string) {
		t.Helper()
		for view, output := range outputs {
			for _, want := range present {
				if !strings.Contains(output, want) {
					t.Errorf("%s: %s omitted %q:\n%s", step, view, want, output)
				}
			}
			for _, unwanted := range absent {
				if strings.Contains(output, unwanted) {
					t.Errorf("%s: %s unexpectedly contains %q:\n%s", step, view, unwanted, output)
				}
			}
		}
	}

	// Working: the observation starts its own clock.
	waitStatus("working")
	expectEvery("working", views(), []string{"working", "observed-agent"}, []string{"agent_mismatch", "no pane"})
	if list := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "list", "--json"); !strings.Contains(list, `"agent":"working `) && !strings.Contains(list, `"agent": "working `) {
		t.Fatalf("posse list --json lacks a working agent observation with its age: %s", list)
	}

	// Done turn: Herdr reports an agent that ended its turn in an unfocused
	// pane as done. That is an observation, never the Task's done state, and
	// its age comes from the recorded worker_idle clock, not one sample.
	report("idle")
	waitStatus("done")
	runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "list")
	task = fixture.mustTask(t, "t1")
	if task.AgentStatus != "done" || task.IdleSince == 0 {
		t.Fatalf("done-turn observation was not recorded: status=%q idle_since=%d", task.AgentStatus, task.IdleSince)
	}
	backdated := time.Now().Add(-(2*time.Hour + 5*time.Minute)).UnixMilli()
	if _, err := fixture.db.ExecContext(ctx, `UPDATE tasks SET idle_since=?, agent_status_since=? WHERE id=?`, backdated, backdated, task.ID); err != nil {
		t.Fatal(err)
	}
	expectEvery("done turn", views(), []string{"done 2h5m", "agent_mismatch", "Task working, but agent idle without a Signal past idle_after 3m"}, []string{"state: done"})
	if state := fixture.mustTask(t, "t1").State; state != store.StateWorking {
		t.Fatalf("Task state = %s after a Herdr done turn; want working", state)
	}

	// Idle: once the pane is seen Herdr reports idle; the same idle episode
	// keeps its age and the mismatch.
	if _, err := client.Call(ctx, "pane.focus", map[string]any{"pane_id": task.PaneID}); err != nil {
		t.Fatal(err)
	}
	waitStatus("idle")
	expectEvery("idle", views(), []string{"idle 2h5m", "Task working, but agent idle without a Signal past idle_after 3m"}, nil)
	if state := fixture.mustTask(t, "t1").State; state != store.StateWorking {
		t.Fatalf("Task state = %s after a long idle observation; want working", state)
	}

	// Resuming work clears the mismatch and restarts the age.
	report("working")
	waitStatus("working")
	expectEvery("resumed", views(), []string{"working "}, []string{"agent_mismatch", "2h5m"})

	// Missing pane: within the absence grace the Task is still live.
	if _, err := client.Call(ctx, "pane.close", map[string]any{"pane_id": task.PaneID}); err != nil {
		t.Fatal(err)
	}
	outputs := views()
	if state := fixture.mustTask(t, "t1").State; state != store.StateWorking {
		t.Fatalf("Task state = %s right after its pane closed; want working inside the absence grace", state)
	}
	expectEvery("missing pane", outputs, []string{"no pane"}, []string{"agent_mismatch"})
}
