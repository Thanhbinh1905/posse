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

func leadWorkspaceLabel(project store.Project) string { return "Lead:" + project.Name }

// Recorded ids are accepted only when the Lead pane or workspace label still
// identifies the workspace. Herdr can reuse ids after restart.
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

// openRiderTab is the placement seam: repository Mounts become linked child
// workspaces (ADR 0012); Workspace Projects keep their shared tabs (ADR 0009).
// The pane is labeled before returning so recovery and Teardown can find it.
func (s *Service) openRiderTab(ctx context.Context, home string, project store.Project, task store.Task, path, displayAgent string) (openedTab, error) {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return openedTab{}, err
	}
	workspaceID, found := leadWorkspace(snapshot, project)
	if !found {
		return openedTab{}, axi.Failure("lead_missing", "the Lead's Herdr workspace is not open", false, "Run `posse up` to restart the Lead, then retry")
	}
	if !project.IsWorkspace() {
		for _, workspace := range snapshot.Workspaces {
			if workspace.Worktree.CheckoutPath == path || workspace.Root == path {
				return openedTab{}, axi.Failure("rider_workspace_occupied", "another Herdr workspace already holds this Mount", false, "Inspect the Mount's open panes before retrying")
			}
		}
	}
	method := "tab.create"
	params := map[string]any{
		"workspace_id": workspaceID, "cwd": path, "label": workerTabLabel(task), "focus": false,
		"env": map[string]string{workerHomeEnv: filepath.Clean(home)},
	}
	if !project.IsWorkspace() {
		method = "worktree.open"
		params = map[string]any{"workspace_id": workspaceID, "path": path, "label": workerTabLabel(task), "focus": false}
	}
	raw, err := s.herdrCall(ctx, method, params)
	if err != nil {
		return openedTab{}, err
	}
	var result struct {
		AlreadyOpen bool `json:"already_open"`
		Workspace   struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
			TabID  string `json:"tab_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.RootPane.PaneID == "" {
		return openedTab{}, axi.Failure("herdr_invalid_response", "Herdr did not return the Rider's pane", true)
	}
	if !project.IsWorkspace() {
		if result.AlreadyOpen {
			return openedTab{}, axi.Failure("rider_workspace_occupied", "the Mount was opened before Posse could label its pane", false, "Inspect the Mount's open panes before retrying")
		}
		workspaceID = result.Workspace.WorkspaceID
		if workspaceID == "" || workspaceID == project.HerdrWorkspaceID {
			return openedTab{}, axi.Failure("herdr_invalid_response", "Herdr did not return a linked Rider workspace", true)
		}
	}
	opened := openedTab{WorkspaceID: workspaceID, TabID: result.RootPane.TabID, PaneID: result.RootPane.PaneID}
	if opened.TabID == "" {
		opened.TabID = result.Tab.TabID
	}
	task.PaneID = opened.PaneID
	metadata := workerDisplayMetadata(task, "", displayAgent)
	// Linked repository workspaces already supply the visible Rider name.
	row := ""
	if project.IsWorkspace() {
		row = workerTabLabel(task)
		row = riderTabLabel(row, true)
	}
	metadata["tokens"].(map[string]string)["posse_row"] = row
	if _, err := s.herdrCall(ctx, "pane.report_metadata", metadata); err != nil {
		return openedTab{}, err
	}
	if _, err := s.herdrCall(ctx, "pane.rename", map[string]any{"pane_id": opened.PaneID, "label": task.PaneLabel}); err != nil {
		return openedTab{}, err
	}
	return opened, nil
}

func findTaskPane(panes []herdr.Pane, task store.Task) (herdr.Pane, bool) {
	pane, found := herdr.FindPane(panes, task.PaneID, task.PaneLabel)
	if found && pane.Label == "" && !pathInside(pane.CWD, task.WorktreePath) {
		return herdr.Pane{}, false
	}
	return pane, found
}
