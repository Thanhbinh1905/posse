//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Simulate a Rider opened before ADR 0012 by moving its running pane into a
// Lead tab without changing the Task's Mount or agent session.
func (f *riderTabsFixture) rideLegacy(t *testing.T, id, title, name string) store.Task {
	t.Helper()
	task := f.ride(t, id, title, name)
	return f.moveRiderToLegacyTab(t, task)
}

func (f *riderTabsFixture) moveRiderToLegacyTab(t *testing.T, task store.Task) store.Task {
	t.Helper()
	focusedBefore := f.snapshot(t).FocusedPaneID
	raw, err := f.client.Call(context.Background(), "pane.move", map[string]any{
		"pane_id":     task.PaneID,
		"destination": map[string]any{"type": "new_tab", "workspace_id": f.leadWorkspaceID, "label": task.ShortName},
		"focus":       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	var moved struct {
		MoveResult struct {
			Pane herdr.Pane `json:"pane"`
		} `json:"move_result"`
	}
	if f.snapshot(t).FocusedPaneID != focusedBefore {
		t.Fatal("unfocused Rider move stole focus")
	}
	if err := json.Unmarshal(raw, &moved); err != nil || moved.MoveResult.Pane.PaneID == "" {
		t.Fatalf("move legacy Rider: %s %v", raw, err)
	}
	id := fmt.Sprintf("t%d", task.Seq)
	if !waitForCondition(10*time.Second, func() bool {
		runPosse(t, f.binary, f.repo, f.env, "roster")
		current := f.task(t, id)
		return current.PaneID == moved.MoveResult.Pane.PaneID
	}) {
		t.Fatalf("reconcile did not follow moved Rider pane %s: %#v", moved.MoveResult.Pane.PaneID, f.task(t, id))
	}
	return f.task(t, id)
}
