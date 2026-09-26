package app

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type openedTab struct {
	WorkspaceID string
	TabID       string
	PaneID      string
}

// leadWorkspaceLabel is the label `posse up` and Lead recovery give the Lead's workspace.
func leadWorkspaceLabel(project store.Project) string { return "Lead:" + project.Name }

// leadWorkspace returns the Lead's workspace in a fresh snapshot. Herdr reuses
// workspace ids after a restart, so a recorded id is accepted only when the
// Lead pane is still in it or it still carries the Lead workspace label.
func leadWorkspace(snapshot herdr.Snapshot, project store.Project) (string, bool) {
	if project.LeadLabel != "" {
		for _, pane := range snapshot.Panes {
			if pane.Label == project.LeadLabel {
				return pane.WorkspaceID, true
			}
		}
	}
	if project.HerdrWorkspaceID == "" {
		return "", false
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == project.LeadPaneID && pane.WorkspaceID == project.HerdrWorkspaceID && (pane.Label == "" || pane.Label == project.LeadLabel) {
			return pane.WorkspaceID, true
		}
	}
	for _, workspace := range snapshot.Workspaces {
		if workspace.WorkspaceID == project.HerdrWorkspaceID && workspace.Label == leadWorkspaceLabel(project) {
			return workspace.WorkspaceID, true
		}
	}
	return "", false
}

// openRiderTab opens a Mount as a new unfocused tab of the Lead's workspace
// (ADR 0009). It never creates a workspace or uses worktree.open, and it labels
// the pane before returning so an interrupted command leaves a pane that
// recovery and Teardown can recognize. POSSE_WORKER_HOME marks the pane as a
// Worker of this home (section 13).
func (s *Service) openRiderTab(ctx context.Context, home string, project store.Project, task store.Task, path string) (openedTab, error) {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return openedTab{}, err
	}
	workspaceID, found := leadWorkspace(snapshot, project)
	if !found {
		return openedTab{}, axi.Failure("lead_missing", "the Lead's Herdr workspace is not open", false, "Run `posse up` to restart the Lead, then retry")
	}
	raw, err := s.herdrCall(ctx, "tab.create", map[string]any{
		"workspace_id": workspaceID, "cwd": path, "label": workerTabLabel(task), "focus": false,
		"env": map[string]string{workerHomeEnv: filepath.Clean(home)},
	})
	if err != nil {
		return openedTab{}, err
	}
	var result struct {
		Tab struct {
			TabID       string `json:"tab_id"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID      string `json:"pane_id"`
			TabID       string `json:"tab_id"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.RootPane.PaneID == "" {
		detail := "response is missing root_pane.pane_id"
		if err != nil {
			detail = err.Error()
		}
		return openedTab{}, axi.Failure("herdr_invalid_response", "Herdr did not return the Rider's tab and pane", true, detail)
	}
	opened := openedTab{WorkspaceID: workspaceID, TabID: result.RootPane.TabID, PaneID: result.RootPane.PaneID}
	if opened.TabID == "" {
		opened.TabID = result.Tab.TabID
	}
	if _, err := s.herdrCall(ctx, "pane.rename", map[string]any{"pane_id": opened.PaneID, "label": task.PaneLabel}); err != nil {
		// The tab holds only the pane just created, and nothing else can find it unlabeled.
		if opened.TabID != "" {
			_, _ = s.herdrCall(ctx, "tab.close", map[string]any{"tab_id": opened.TabID})
		}
		return openedTab{}, err
	}
	return opened, nil
}

// findTaskPane finds a Task's pane by its label. Relaunch starts an agent in
// the pane it finds, so an unlabeled pane with the recorded id must also be
// inside the Task's Mount.
func findTaskPane(panes []herdr.Pane, task store.Task) (herdr.Pane, bool) {
	pane, found := herdr.FindPane(panes, task.PaneID, task.PaneLabel)
	if found && pane.Label == "" && !pathInside(pane.CWD, task.WorktreePath) {
		return herdr.Pane{}, false
	}
	return pane, found
}
