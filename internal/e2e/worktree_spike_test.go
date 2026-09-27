//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
)

func TestExistingMountWorktreeOpenAndLockSpike(t *testing.T) {
	f := newRiderTabsFixture(t)
	task := f.ride(t, "t1", "Spike rider", "spike-rider")
	// The existing tab does not make the Mount a Herdr worktree workspace.
	raw, err := f.client.Call(context.Background(), "worktree.open", map[string]any{"workspace_id": f.leadWorkspaceID, "path": task.WorktreePath, "label": "spike-rider", "focus": false})
	if err != nil {
		t.Fatal(err)
	}
	var opened struct {
		Workspace herdr.Workspace `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &opened); err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot(t)
	t.Logf("open=%s workspace=%+v focus=%s", raw, opened.Workspace, snap.FocusedPaneID)
	if opened.Workspace.WorkspaceID == "" || opened.Workspace.WorkspaceID == f.leadWorkspaceID || opened.Workspace.Worktree.CheckoutPath != task.WorktreePath || snap.FocusedPaneID != f.leadPaneID {
		t.Fatalf("not an unfocused linked workspace: %+v %+v", opened, snap)
	}
	lock := exec.Command("git", "-C", f.repo, "worktree", "list", "--porcelain")
	lock.Env = f.env
	if output, err := lock.CombinedOutput(); err != nil || !strings.Contains(string(output), "locked posse: held by t1") {
		t.Fatalf("held Mount must be locked: %s: %v", output, err)
	}
	for _, force := range []bool{false, true} {
		_, err := f.client.Call(context.Background(), "worktree.remove", map[string]any{"workspace_id": opened.Workspace.WorkspaceID, "force": force})
		if err == nil {
			t.Fatalf("locked Mount removed with force=%v", force)
		}
		t.Logf("remove(force=%v) refused: %v", force, err)
	}
	snap = f.snapshot(t)
	if snap.FocusedPaneID != f.leadPaneID {
		t.Fatalf("focus stolen: %s", snap.FocusedPaneID)
	}
}
