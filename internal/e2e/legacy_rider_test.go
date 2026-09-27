//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

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
	if err := json.Unmarshal(raw, &moved); err != nil || moved.MoveResult.Pane.PaneID == "" {
		t.Fatalf("move legacy Rider: %s %v", raw, err)
	}
	runPosse(t, f.binary, f.repo, f.env, "roster")
	return f.task(t, fmt.Sprintf("t%d", task.Seq))
}
