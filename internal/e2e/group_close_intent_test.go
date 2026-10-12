//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

// pauseCommand holds a real CLI process after a durable intent step. Only the
// isolated fixture gets the test pause; the User's Herdr session is untouched.
func pauseCommand(t *testing.T, f *riderTabsFixture, point string, action func(), args ...string) (string, error) {
	t.Helper()
	marker := filepath.Join(f.root, "intent-pause")
	env := setEnv(f.leadEnv, "POSSE_INTENT_PAUSE_AT", point)
	env = setEnv(env, "POSSE_INTENT_PAUSE_FILE", marker)
	cmd := exec.Command(f.binary, args...)
	cmd.Env, cmd.Dir = env, f.repo
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(marker+".continue", nil, 0o600)
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	if !waitForCondition(20*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
		t.Fatalf("%v did not pause at %s: %q", args, point, output.String())
	}
	action()
	if err := os.WriteFile(marker+".continue", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	return output.String(), err
}

func closeRiderGroup(t *testing.T, f *riderTabsFixture) {
	t.Helper()
	if _, err := f.client.Call(context.Background(), "workspace.close", map[string]any{"workspace_id": f.leadWorkspaceID, "close_group": true}); err != nil {
		t.Fatal(err)
	}
	if _, present := findLeadInSnapshot(f.snapshot(t)); present {
		t.Fatal("group close kept the Lead")
	}
}

func restoreRiderGroup(t *testing.T, f *riderTabsFixture) {
	t.Helper()
	runPosse(t, f.binary, f.repo, f.env, "recover", "--all")
	if !waitForCondition(20*time.Second, func() bool { _, found := findLeadInSnapshot(f.snapshot(t)); return found }) {
		t.Fatal("Lead was not recovered after group close")
	}
}

func TestGroupCloseDuringRideRecoversLeadAndMount(t *testing.T) {
	f := newRiderTabsFixture(t)
	brief := filepath.Join(f.root, "group-ride.md")
	if err := os.WriteFile(brief, []byte("---\ntype: ship\ntitle: Group ride\ndone_when: test finishes\n---\nWait.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := pauseCommand(t, f, "ride:after:pane.record", func() { closeRiderGroup(t, f) }, "ride", "--brief", brief, "--name", "group-ride")
	if err == nil {
		if !strings.Contains(output, "state: working") {
			t.Fatalf("ride succeeded without a working Task: %s", output)
		}
	} else if !strings.Contains(output, `"pane_not_found"`) || !strings.Contains(output, ",true,") || !strings.Contains(output, "posse recover --all") {
		t.Fatalf("ride failed without a recoverable pane-close error: %s %v", output, err)
	}
	restoreRiderGroup(t, f)
	task := f.task(t, "t1")
	if task.State != store.StateWorking && task.State != store.StateFailed && task.State != store.StateLost {
		t.Fatalf("interrupted ride left Task in unexpected state %s", task.State)
	}
	if task.MountID != 0 {
		reason := gitTest(t, f.env, f.repo, "worktree", "list", "--porcelain")
		if !strings.Contains(reason, "locked posse: held by t1") {
			t.Fatalf("held Rider Mount lost its lock: %s", reason)
		}
	} else if task.State == store.StateWorking {
		t.Fatal("working Task lost its Mount")
	}
	// Recovery can move the Lead to a different workspace. Close that
	// isolated group so its panes do not outlive the fixture server.
	lead, found := findLeadInSnapshot(f.snapshot(t))
	if !found {
		t.Fatal("recovered Lead pane missing before cleanup")
	}
	if _, err := f.client.Call(context.Background(), "workspace.close", map[string]any{"workspace_id": lead.WorkspaceID, "close_group": true}); err != nil {
		t.Fatalf("close recovered group: %v", err)
	}
	if _, present := findLeadInSnapshot(f.snapshot(t)); present {
		t.Fatal("recovered group still holds the Lead after close")
	}
}

func TestGroupCloseDuringRelaunchRestoresLeadAndRider(t *testing.T) {
	f := newRiderTabsFixture(t)
	before := f.ride(t, "t1", "Group relaunch", "group-relaunch")
	f.fail(t, "t1")
	output, commandErr := pauseCommand(t, f, "relaunch:after:pane.label", func() { closeRiderGroup(t, f) }, "relaunch", "t1")
	if commandErr == nil {
		if !strings.Contains(output, "t1") {
			t.Fatalf("relaunch succeeded without reporting Task t1: %s", output)
		}
	} else if (!strings.Contains(output, `"agent_pane_not_found"`) && !strings.Contains(output, `"pane_not_found"`)) || !strings.Contains(output, ",true,") || !strings.Contains(output, "posse recover --all") {
		t.Fatalf("relaunch failed without a recoverable pane-close error: %s %v", output, commandErr)
	}
	restoreRiderGroup(t, f)
	after := f.task(t, "t1")
	if after.MountID != before.MountID {
		t.Fatalf("relaunch lost held Mount: before=%#v after=%#v", before, after)
	}
	if after.State != store.StateWorking && after.State != store.StateFailed && after.State != store.StateLost {
		t.Fatalf("interrupted relaunch left Task in unexpected state %s", after.State)
	}
	if after.State == store.StateFailed || after.State == store.StateLost {
		lead, found := findLeadInSnapshot(f.snapshot(t))
		if !found {
			t.Fatal("recovered Lead pane missing")
		}
		leadEnv := setEnv(f.leadEnv, "HERDR_PANE_ID", lead.PaneID)
		leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", lead.WorkspaceID)
		leadEnv = setEnv(leadEnv, "HERDR_TAB_ID", lead.TabID)
		runPosse(t, f.binary, f.repo, leadEnv, "relaunch", "t1")
	}
	f.waitState(t, "t1", store.StateWorking)
}

func TestGroupCloseDuringTeardownRestoresSurvivingRider(t *testing.T) {
	f := newRiderTabsFixture(t)
	first := f.ride(t, "t1", "Group teardown", "group-teardown")
	second := f.ride(t, "t2", "Surviving rider", "surviving-rider")
	f.fail(t, "t1")
	output, err := pauseCommand(t, f, "unsaddle:after:panes.close", func() { closeRiderGroup(t, f) }, "unsaddle", "t1", "--discard", "--user-approved", "Test approved teardown")
	if err != nil {
		t.Fatalf("teardown after group close: %s %v", output, err)
	}
	restoreRiderGroup(t, f)
	if state := f.task(t, "t1").State; state != store.StateTornDown {
		t.Fatalf("teardown state = %s", state)
	}
	assertMountUnlocked(t, f.env, f.repo, first.WorktreePath)
	if !waitForCondition(20*time.Second, func() bool { return f.task(t, "t2").Launches > second.Launches }) {
		t.Fatal("surviving Rider was not recovered")
	}
}
