//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRidersAsGroupedChildrenRecoverAfterAnotherPrimaryClosesGroup(t *testing.T) {
	f := newRiderTabsFixture(t)
	// Install only the event hook, on this isolated server and home.
	manifest := filepath.Join(f.root, "group-recovery-plugin", "herdr-plugin.toml")
	if err := os.MkdirAll(filepath.Dir(manifest), 0700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("id = \"posse.group-e2e\"\nname = \"Group recovery E2E\"\nversion = \"0.1.0\"\nmin_herdr_version = \"0.9.0\"\nplatforms = [\"linux\"]\n[[events]]\non = \"workspace.closed\"\ncommand = [%q, \"_ingest\"]\n", f.binary)
	if err := os.WriteFile(manifest, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Call(context.Background(), "plugin.link", map[string]any{"path": manifest, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	first := f.ride(t, "t1", "Native first", "native-first")
	second := f.ride(t, "t2", "Native second", "native-second")
	snap := f.snapshot(t)
	if snap.FocusedPaneID != f.leadPaneID {
		t.Fatalf("ride stole focus: %s", snap.FocusedPaneID)
	}
	leadRepoKey := ""
	for _, w := range snap.Workspaces {
		if w.WorkspaceID == f.leadWorkspaceID {
			leadRepoKey = w.Worktree.RepoKey
		}
	}
	if leadRepoKey == "" {
		t.Fatal("Lead was not promoted to the worktree group primary")
	}
	for _, task := range []store.Task{first, second} {
		if task.HerdrWorkspaceID == f.leadWorkspaceID {
			t.Fatalf("Rider %d opened as tab", task.Seq)
		}
		found := false
		for _, w := range snap.Workspaces {
			if w.WorkspaceID == task.HerdrWorkspaceID {
				found = w.Worktree.IsLinkedWorktree && w.Worktree.RepoKey == leadRepoKey && w.Worktree.CheckoutPath == task.WorktreePath && w.Label == task.ShortName
			}
		}
		if !found {
			t.Fatalf("Rider is not a linked child: %#v", snap.Workspaces)
		}
		for _, tab := range snap.Tabs {
			if tab.WorkspaceID == task.HerdrWorkspaceID && tab.Label == task.ShortName {
				t.Fatalf("child tab duplicates workspace label")
			}
		}
	}
	assertNativeWorktreeSidebar(t, f, first.ShortName, second.ShortName)
	if f.snapshot(t).FocusedPaneID != f.leadPaneID {
		t.Fatal("sidebar client stole focus")
	}
	// Leave uncommitted work in the Mount. Recovery must not reset the checkout.
	changed := filepath.Join(first.WorktreePath, "unfinished.txt")
	if err := os.WriteFile(changed, []byte("keep this work"), 0600); err != nil {
		t.Fatal(err)
	}
	strayPath := filepath.Join(f.root, "stray-linked")
	gitTest(t, f.env, f.repo, "worktree", "add", "--detach", strayPath)
	defer gitTest(t, f.env, f.repo, "worktree", "remove", "--force", strayPath)
	stray := createLabeledWorkspace(t, f.client, f.repo, "stray-primary")
	if _, err := f.client.Call(context.Background(), "worktree.open", map[string]any{"workspace_id": stray.Workspace.WorkspaceID, "path": strayPath, "focus": false}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Call(context.Background(), "workspace.close", map[string]any{"workspace_id": stray.Workspace.WorkspaceID, "close_group": true}); err != nil {
		t.Fatal(err)
	}
	if !waitForCondition(30*time.Second, func() bool {
		a, b := f.task(t, "t1"), f.task(t, "t2")
		snap := f.snapshot(t)
		_, ok := findLeadInSnapshot(snap)
		return ok && a.Launches > first.Launches && b.Launches > second.Launches
	}) {
		logs, logErr := f.client.Call(context.Background(), "plugin.log.list", map[string]any{"plugin_id": "posse.group-e2e"})
		t.Logf("hook logs=%s error=%v", logs, logErr)
		t.Fatalf("group close did not automatically restore Lead and Riders: %#v %#v", f.task(t, "t1"), f.task(t, "t2"))
	}
	snap = f.snapshot(t)
	if !waitForCondition(30*time.Second, func() bool {
		db, err := store.OpenReadOnly(f.home)
		if err != nil {
			return false
		}
		defer db.Close()
		project, err := db.ProjectByID(context.Background(), f.projectID)
		return err == nil && project.LeadPaneID != ""
	}) {
		t.Fatal("Lead recovery did not finish")
	}
	if data, err := os.ReadFile(changed); err != nil || string(data) != "keep this work" {
		t.Fatalf("unfinished work lost: %q %v", data, err)
	}
	if snap.FocusedPaneID == f.task(t, "t1").PaneID || snap.FocusedPaneID == f.task(t, "t2").PaneID {
		t.Fatalf("recovery focused a Rider: %s", snap.FocusedPaneID)
	}
	for _, task := range []store.Task{f.task(t, "t1"), f.task(t, "t2")} {
		present := false
		for _, w := range snap.Workspaces {
			if w.WorkspaceID == task.HerdrWorkspaceID && w.Worktree.CheckoutPath == task.WorktreePath {
				present = true
			}
		}
		if !present {
			t.Fatalf("recovered child lost grouping: %#v", task)
		}
	}
	db, err := store.OpenReadOnly(f.home)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(context.Background(), f.projectID, false)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	recoveredNotice := false
	for _, n := range notices {
		if n.Kind == "recovery" && strings.Contains(n.Summary, "group close") {
			recoveredNotice = true
		}
	}
	if !recoveredNotice {
		t.Fatalf("no group-close recovery Notice: %#v", notices)
	}
	// A normal Teardown closes only the child and unlocks its released Mount.
	second = f.task(t, "t2")
	f.fail(t, "t2")
	lead, _ := findLeadInSnapshot(snap)
	leadEnv := setEnv(f.leadEnv, "HERDR_PANE_ID", lead.PaneID)
	leadEnv = setEnv(leadEnv, "HERDR_WORKSPACE_ID", lead.WorkspaceID)
	if _, err := f.client.Call(context.Background(), "pane.focus", map[string]any{"pane_id": f.userBefore.RootPane.PaneID}); err != nil {
		t.Fatal(err)
	}
	focusedBeforeClose := f.snapshot(t).FocusedPaneID
	runPosse(t, f.binary, f.repo, leadEnv, "unsaddle", "t2", "--discard", "--user-approved", "User approved the child teardown test")
	snap = f.snapshot(t)
	if snap.FocusedPaneID != focusedBeforeClose {
		t.Fatalf("Teardown stole focus: before %s after %s", focusedBeforeClose, snap.FocusedPaneID)
	}
	for _, w := range snap.Workspaces {
		if w.WorkspaceID == second.HerdrWorkspaceID {
			t.Fatalf("teardown left child open")
		}
	}
	if _, ok := findLeadInSnapshot(snap); !ok {
		t.Fatal("teardown closed the Lead")
	}
	f.assertAlive(t, snap, f.task(t, "t1").PaneID)
	list := exec.Command("git", "-C", f.repo, "worktree", "list", "--porcelain")
	list.Env = f.env
	output, err := list.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range strings.Split(string(output), "\n\n") {
		if strings.HasPrefix(block, "worktree "+second.WorktreePath+"\n") && strings.Contains(block, "locked ") {
			t.Fatalf("released Mount still locked: %s", block)
		}
	}
}

// A real attached Herdr 0.9.0 client must render the grouped names without
// any Posse-specific sidebar configuration.
func assertNativeWorktreeSidebar(t *testing.T, f *riderTabsFixture, names ...string) {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Log("script unavailable; snapshot still verifies grouping")
		return
	}
	cmd := exec.Command("script", "-q", "-c", "stty rows 45 cols 160; timeout 4 herdr", "/dev/null")
	cmd.Env = setEnv(f.env, "TERM", "xterm-256color")
	cmd.Dir = f.repo
	input, keepOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer keepOpen.Close()
	cmd.Stdin = input
	output, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(err.Error(), "exit status 124") {
		t.Fatalf("attach isolated client: %v: %q", err, output)
	}
	for _, name := range append([]string{"Lead:shop"}, names...) {
		if !bytes.Contains(output, []byte(name)) {
			t.Fatalf("Herdr sidebar did not render %q; output tail: %q", name, output[max(0, len(output)-4000):])
		}
	}
}

func findLeadInSnapshot(snap herdr.Snapshot) (herdr.Pane, bool) {
	for _, p := range snap.Panes {
		if p.Label == "posse:shop:lead" && p.Agent != "" {
			return p, true
		}
	}
	return herdr.Pane{}, false
}
