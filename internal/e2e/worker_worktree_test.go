//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRepositoryWorkersOpenAsLeadTabsAndTeardownIsScoped(t *testing.T) {
	f := newWorkerTabsFixture(t)
	first := f.start(t, "t1", "Native first", "native-first")
	second := f.start(t, "t2", "Native second", "native-second")
	snapshot := f.snapshot(t)
	t.Logf("Herdr layout after Worker launches: %#v", snapshot)
	lead, found := findLeadInSnapshot(snapshot)
	if !found {
		t.Fatal("Lead pane is missing")
	}
	if snapshot.FocusedPaneID != f.leadPaneID {
		t.Fatalf("Worker start stole focus: %s", snapshot.FocusedPaneID)
	}
	for _, task := range []store.Task{first, second} {
		if task.HerdrWorkspaceID != lead.WorkspaceID {
			t.Fatalf("Worker %d opened outside Lead workspace: task=%#v Lead=%#v", task.Seq, task, lead)
		}
		f.assertWorkerTab(t, snapshot, task)
		for _, workspace := range snapshot.Workspaces {
			if workspace.Worktree.CheckoutPath == task.WorktreePath {
				t.Fatalf("Worker %d created a linked workspace: %#v", task.Seq, workspace)
			}
		}
	}

	// Verify Teardown remains tab-scoped: the first tab closes, while the Lead
	// and sibling Worker tab remain untouched.
	f.fail(t, "t1")
	f.exitWorker(t, "t1")
	firstTab := tabOf(t, f.snapshot(t), first.PaneID)
	if _, err := f.client.Call(context.Background(), "tab.close", map[string]any{"tab_id": firstTab}); err != nil {
		t.Fatalf("close exited Worker tab: %v", err)
	}
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1", "--discard", "--user-approved", "User approved the tab teardown test")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("Teardown failed: %s", output)
	}
	snapshot = f.snapshot(t)
	t.Logf("Herdr layout after t1 Teardown: %#v", snapshot)
	f.assertAlive(t, snapshot, f.leadPaneID, second.PaneID)
	f.assertGone(t, snapshot, first.PaneID)
	if _, found := findLeadInSnapshot(snapshot); !found {
		t.Fatal("Teardown closed the Lead")
	}
}

func TestLegacyLinkedWorkerChildTearsDownWithoutClosingLeadOrSibling(t *testing.T) {
	f := newWorkerTabsFixture(t)
	legacy := f.start(t, "t1", "Legacy linked Worker", "legacy-linked")
	sibling := f.start(t, "t2", "Sibling tab Worker", "sibling-tab")
	f.fail(t, "t1")
	f.exitWorker(t, "t1")
	oldTab := tabOf(t, f.snapshot(t), legacy.PaneID)
	if _, err := f.client.Call(context.Background(), "tab.close", map[string]any{"tab_id": oldTab}); err != nil {
		t.Fatalf("close exited tab before migrating legacy fixture: %v", err)
	}
	raw, err := f.client.Call(context.Background(), "worktree.open", map[string]any{
		"workspace_id": f.leadWorkspaceID, "path": legacy.WorktreePath, "label": legacy.ShortName, "focus": false,
	})
	if err != nil {
		t.Fatalf("recreate legacy linked Worker workspace: %v", err)
	}
	var opened struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &opened); err != nil || opened.Workspace.WorkspaceID == "" || opened.RootPane.PaneID == "" {
		t.Fatalf("legacy linked Worker response = %s: %v", raw, err)
	}
	if _, err := f.client.Call(context.Background(), "pane.rename", map[string]any{"pane_id": opened.RootPane.PaneID, "label": legacy.PaneLabel}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskWorkspace(context.Background(), legacy.ID, opened.Workspace.WorkspaceID, opened.RootPane.PaneID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	// Herdr removes the child's shell when its last tab closes. Posse must still
	// finish legacy Teardown without closing the Lead or the sibling tab.
	if _, err := f.client.Call(context.Background(), "workspace.close", map[string]any{"workspace_id": opened.Workspace.WorkspaceID}); err != nil {
		t.Fatalf("close only the legacy child workspace: %v", err)
	}
	output := runPosse(t, f.binary, f.repo, f.leadEnv, "teardown", "t1", "--discard", "--user-approved", "User approved the legacy linked-child teardown test")
	if !strings.Contains(output, "torn-down") {
		t.Fatalf("legacy linked-child Teardown failed: %s", output)
	}
	snapshot := f.snapshot(t)
	f.assertAlive(t, snapshot, f.leadPaneID, sibling.PaneID)
	f.assertGone(t, snapshot, opened.RootPane.PaneID)
	if _, found := findLeadInSnapshot(snapshot); !found {
		t.Fatal("legacy linked-child Teardown closed the Lead")
	}
}

// A real attached Herdr 0.9.0 client must render the grouped names without
// any Posse-specific sidebar configuration.
func assertNativeWorktreeSidebar(t *testing.T, f *workerTabsFixture, names ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Log("script unavailable; snapshot still verifies grouping")
		return nil
	}
	var output []byte
	for attempt := 0; attempt < 3; attempt++ {
		cmd := exec.Command("script", "-q", "-c", "stty rows 45 cols 160; timeout 4 herdr", "/dev/null")
		cmd.Env = setEnv(f.env, "HERDR_SOCKET_PATH", herdr.IsolatedTestSocketPath(f.root))
		cmd.Env = setEnv(cmd.Env, "TERM", "xterm-256color")
		cmd.Dir = f.repo
		input, keepOpen, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdin = input // An EOF would type Ctrl-D into the active pane.
		output, err = cmd.CombinedOutput()
		_ = input.Close()
		_ = keepOpen.Close()
		if err != nil && !strings.Contains(err.Error(), "exit status 124") {
			t.Fatalf("attach isolated client: %v: %q", err, output)
		}
		if bytes.Contains(output, []byte("Lead:shop")) {
			break
		}
	}
	for _, name := range append([]string{"Lead:shop"}, names...) {
		if !bytes.Contains(output, []byte(name)) {
			log, _ := os.ReadFile(filepath.Join(f.root, "xdg", "herdr", "herdr-client.log"))
			t.Fatalf("Herdr client did not render %q after retries; output tail: %q client log tail: %q", name, output[max(0, len(output)-4000):], log[max(0, len(log)-2500):])
		}
	}
	return output
}

func findLeadInSnapshot(snap herdr.Snapshot) (herdr.Pane, bool) {
	for _, p := range snap.Panes {
		if p.Label == "posse:shop:lead" && p.Agent != "" {
			return p, true
		}
	}
	return herdr.Pane{}, false
}
