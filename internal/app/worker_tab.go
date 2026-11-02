package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

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

func sandboxOccupied(holder string) error {
	return axi.Failure("worker_workspace_occupied", "another Herdr workspace already holds this Sandbox: "+holder, false, "Inspect the Sandbox's open panes before retrying")
}

// openWorkerTab places every Worker Sandbox in an unfocused tab of the Lead's
// workspace. The pane is labeled before returning so recovery and Teardown can find it.
func (s *Service) openWorkerTab(ctx context.Context, home string, project store.Project, task store.Task, path, displayAgent string) (openedTab, error) {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return openedTab{}, err
	}
	workspaceID, found := leadWorkspace(snapshot, project)
	if !found {
		return openedTab{}, axi.Failure("lead_missing", "the Lead's Herdr workspace is not open", false, "Run `posse up` to restart the Lead, then retry")
	}
	for _, workspace := range snapshot.Workspaces {
		if workspace.Root == path {
			return openedTab{}, sandboxOccupied("workspace " + workspace.WorkspaceID + " root")
		}
		if workspace.Worktree.CheckoutPath == path {
			for _, pane := range snapshot.Panes {
				if pane.WorkspaceID == workspace.WorkspaceID {
					return openedTab{}, sandboxOccupied("workspace " + workspace.WorkspaceID + " checkout, pane " + pane.PaneID)
				}
			}
		}
	}
	for _, pane := range snapshot.Panes {
		if filepath.Clean(pane.CWD) != filepath.Clean(path) || pane.PaneID == task.PaneID {
			continue
		}
		otherTaskPane := strings.HasPrefix(pane.Label, "posse:"+project.Name+":t") && pane.Label != task.PaneLabel
		if pane.WorkspaceID != workspaceID || otherTaskPane {
			return openedTab{}, sandboxOccupied("pane " + pane.PaneID + " label " + pane.Label + " in workspace " + pane.WorkspaceID)
		}
	}
	raw, err := s.herdrCall(ctx, "tab.create", map[string]any{
		"workspace_id": workspaceID, "cwd": path, "label": workerTabLabel(task), "focus": false,
		"env": map[string]string{workerHomeEnv: filepath.Clean(home)},
	})
	if err != nil {
		return openedTab{}, err
	}
	var result struct {
		AlreadyOpen bool `json:"already_open"`
		Tab         struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
			TabID  string `json:"tab_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.RootPane.PaneID == "" {
		return openedTab{}, axi.Failure("herdr_invalid_response", "Herdr did not return the Worker's pane", true)
	}
	if result.AlreadyOpen {
		return openedTab{}, axi.Failure("worker_tab_occupied", "the Worker tab was opened before Posse could label its pane", false, "Inspect the Sandbox's open panes before retrying")
	}
	opened := openedTab{WorkspaceID: workspaceID, TabID: result.RootPane.TabID, PaneID: result.RootPane.PaneID}
	if opened.TabID == "" {
		opened.TabID = result.Tab.TabID
	}
	task.PaneID = opened.PaneID
	metadata := workerDisplayMetadata(task, "", displayAgent)
	metadata["tokens"].(map[string]string)["posse_row"] = treeTabLabel(workerTabLabel(task), true)
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

// paneKeepsDetectedAgent reports whether Herdr still holds a detected agent
// record for the pane. After a detected agent exits, Herdr 0.9.1 keeps that
// record and rejects agent.start in the pane, with no call that removes it. A
// record still pending its launch does not block a new start.
func (s *Service) paneKeepsDetectedAgent(ctx context.Context, paneID string) bool {
	raw, err := s.herdrCall(ctx, "agent.get", map[string]any{"target": paneID})
	if err != nil {
		return false
	}
	var response struct {
		Agent struct {
			Agent         string `json:"agent"`
			LaunchPending bool   `json:"launch_pending"`
		} `json:"agent"`
	}
	return json.Unmarshal(raw, &response) == nil && response.Agent.Agent != "" && !response.Agent.LaunchPending
}

// replaceWorkerPane moves a Task whose stopped agent left a detected record
// into a fresh pane. The old pane is closed only while its shell is still the
// foreground.
func (s *Service) replaceWorkerPane(ctx context.Context, home string, project store.Project, task store.Task, old herdr.Pane, kind string) (herdr.Pane, error) {
	opened, err := s.openWorkerTab(ctx, home, project, task, task.WorktreePath, kind)
	if err != nil {
		return herdr.Pane{}, err
	}
	fresh := herdr.Pane{PaneID: opened.PaneID, WorkspaceID: opened.WorkspaceID, TabID: opened.TabID, Label: task.PaneLabel}
	group, err := s.paneAgentGroup(ctx, old.PaneID)
	if err != nil {
		if missingPaneError(err) {
			return fresh, nil
		}
		return fresh, err
	}
	if group != 0 {
		return fresh, agentStopPaneChanged(old.PaneID)
	}
	if _, err := s.herdrCall(ctx, "pane.close", map[string]any{"pane_id": old.PaneID}); err != nil && !missingPaneError(err) {
		return fresh, err
	}
	return fresh, nil
}
