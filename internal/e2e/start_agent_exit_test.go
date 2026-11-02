//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/store"
)

// An agent that exits before its Brief is delivered fails the start with a
// task_failed Notice. The Lead relaunches it without a discard Decision, and
// the new agent runs in a fresh pane.
func TestStartAgentExitBeforeBriefIsRelaunchable(t *testing.T) {
	fixture := newPRLifecycleFixture(t)
	defer fixture.db.Close()

	marker := filepath.Join(fixture.root, "exit-before-brief")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(fixture.root, "exit-before-brief.md")
	if err := os.WriteFile(brief, []byte("---\ntype: change\ntitle: Exit before Brief\ndone_when: the Worker runs\n---\nStart a Worker that quits at once.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := exec.Command(fixture.binary, "start", "--brief", brief, "--name", "exit-before-brief")
	start.Dir, start.Env = fixture.repo, fixture.leadEnv
	if output, err := start.CombinedOutput(); err == nil {
		t.Fatalf("start succeeded although the agent exited before its Brief:\n%s", output)
	}
	failed := fixture.mustTask(t, "t1")
	if failed.State != store.StateFailed {
		t.Fatalf("Task state = %s, want failed", failed.State)
	}
	notices, err := fixture.db.Notices(context.Background(), fixture.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, notice := range notices {
		reported = reported || notice.Kind == "task_failed"
	}
	if !reported {
		t.Fatalf("no task_failed Notice for the Lead: %#v", notices)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if output := runPosse(t, fixture.binary, fixture.repo, fixture.leadEnv, "relaunch", "t1"); !strings.Contains(output, "t1") {
		t.Fatalf("relaunch: %s", output)
	}
	relaunched := fixture.mustTask(t, "t1")
	if relaunched.State != store.StateWorking || relaunched.PaneID == failed.PaneID {
		t.Fatalf("relaunch: state=%s pane %s -> %s, want working in a fresh pane", relaunched.State, failed.PaneID, relaunched.PaneID)
	}
	decisions, err := fixture.db.Decisions(context.Background(), fixture.project.ID, true)
	if err != nil || len(decisions) != 0 {
		t.Fatalf("a relaunch must not need a Decision: %#v %v", decisions, err)
	}
}
