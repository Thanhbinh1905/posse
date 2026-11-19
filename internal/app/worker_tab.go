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

func leadWorkspace(snapshot herdr.Snapshot, project store.Project) (string, bool) {
	if project.HerdrWorkspaceID == "" {
		return "", false
	}
	for _, pane := range snapshot.Panes {
		if pane.WorkspaceID == project.HerdrWorkspaceID {
			return project.HerdrWorkspaceID, true
		}
	}
	if project.LeadPaneID == "" {
		for _, workspace := range snapshot.Workspaces {
			if workspace.WorkspaceID == project.HerdrWorkspaceID {
				return project.HerdrWorkspaceID, true
			}
		}
	}
	return "", false
}

func sandboxOccupied(holder string) error {
	return axi.Failure("worker_workspace_occupied", "another Herdr workspace already holds this Sandbox: "+holder, false, "Inspect the Sandbox's open panes before retrying")
}

func (s *Service) openWorkerTab(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task, path, displayAgent string) (openedTab, error) {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return openedTab{}, err
	}
	workspaceID, found := leadWorkspace(snapshot, project)
	if !found {
		return openedTab{}, axi.Failure("lead_missing", "the Lead's Herdr workspace is not open", false, "Run `posse up` to restart the Lead, then retry")
	}
	reuseWorkerWorkspace := false
	for _, workspace := range snapshot.Workspaces {
		matchesPath := workspace.Root == path
		if project.IsWorkspace() {
			for _, pane := range snapshot.Panes {
				if pane.WorkspaceID == workspace.WorkspaceID && filepath.Clean(pane.CWD) == filepath.Clean(path) {
					matchesPath = true
					break
				}
			}
		} else {
			matchesPath = workspace.Worktree.CheckoutPath == path || workspace.Root == path
		}
		if !matchesPath {
			continue
		}
		isOwnWorkspace := workspace.WorkspaceID == task.HerdrWorkspaceID && workspace.WorkspaceID != project.HerdrWorkspaceID &&
			(project.IsWorkspace() && matchesPath ||
				!project.IsWorkspace() && workspace.Worktree.IsLinkedWorktree && filepath.Clean(workspace.Worktree.CheckoutPath) == filepath.Clean(path))
		if isOwnWorkspace {
			workspaceID = workspace.WorkspaceID
			reuseWorkerWorkspace = true
			continue
		}
		for _, pane := range snapshot.Panes {
			if pane.WorkspaceID == workspace.WorkspaceID {
				return openedTab{}, sandboxOccupied("workspace " + workspace.WorkspaceID + ", pane " + pane.PaneID)
			}
		}
		return openedTab{}, sandboxOccupied("workspace " + workspace.WorkspaceID)
	}
	for _, pane := range snapshot.Panes {
		if filepath.Clean(pane.CWD) != filepath.Clean(path) || pane.PaneID == task.PaneID {
			continue
		}
		otherTaskPane := strings.HasPrefix(pane.Label, "posse:"+project.Name+":t") && pane.Label != task.PaneLabel
		if (!project.IsWorkspace() && !reuseWorkerWorkspace) || pane.WorkspaceID != workspaceID || otherTaskPane {
			return openedTab{}, sandboxOccupied("pane " + pane.PaneID + " label " + pane.Label + " in workspace " + pane.WorkspaceID)
		}
	}
	method := "tab.create"
	params := map[string]any{
		"workspace_id": workspaceID, "cwd": path, "label": workerTabLabel(task), "focus": false,
		"env": map[string]string{workerHomeEnv: filepath.Clean(home)},
	}
	if project.IsWorkspace() && !reuseWorkerWorkspace {
		method = "workspace.create"
		params = map[string]any{
			"cwd": path, "label": workerTabLabel(task), "focus": false, "no_focus": true,
			"env": map[string]string{workerHomeEnv: filepath.Clean(home)},
		}
	} else if !project.IsWorkspace() && !reuseWorkerWorkspace {
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
		return openedTab{}, axi.Failure("herdr_invalid_response", "Herdr did not return the Worker's pane", true)
	}
	if result.AlreadyOpen {
		return openedTab{}, axi.Failure("worker_tab_occupied", "the Worker tab was opened before Posse could label its pane", false, "Inspect the Sandbox's open panes before retrying")
	}
	if !reuseWorkerWorkspace && (!project.IsWorkspace() || method == "workspace.create") {
		if result.AlreadyOpen || result.Workspace.WorkspaceID == "" || result.Workspace.WorkspaceID == project.HerdrWorkspaceID {
			return openedTab{}, axi.Failure("worker_workspace_occupied", "Herdr did not create a new Worker workspace", false, "Inspect the Sandbox's open panes before retrying")
		}
		workspaceID = result.Workspace.WorkspaceID
	}
	opened := openedTab{WorkspaceID: workspaceID, TabID: result.RootPane.TabID, PaneID: result.RootPane.PaneID}
	if opened.TabID == "" {
		opened.TabID = result.Tab.TabID
	}
	task.PaneID = opened.PaneID
	metadata := workerDisplayMetadata(task, "", displayAgent)
	if _, err := s.herdrCall(ctx, "pane.report_metadata", metadata); err != nil {
		s.closeUnlabeledWorker(ctx, project, task, opened)
		return openedTab{}, err
	}
	if _, err := s.herdrCall(ctx, "pane.rename", map[string]any{"pane_id": opened.PaneID, "label": task.PaneLabel}); err != nil {
		s.closeUnlabeledWorker(ctx, project, task, opened)
		return openedTab{}, err
	}
	if db != nil {
		s.placeWorkerWorkspace(ctx, db, home, project.ID, task.ID, project.HerdrWorkspaceID, opened.WorkspaceID)
	}
	return opened, nil
}

func (s *Service) closeUnlabeledWorker(ctx context.Context, project store.Project, task store.Task, opened openedTab) {
	if opened.WorkspaceID != "" && opened.WorkspaceID != task.HerdrWorkspaceID {
		_, _ = s.herdrCall(ctx, "workspace.close", map[string]any{"workspace_id": opened.WorkspaceID})
	} else if opened.TabID != "" {
		_, _ = s.herdrCall(ctx, "tab.close", map[string]any{"tab_id": opened.TabID})
	}
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
func (s *Service) replaceWorkerPane(ctx context.Context, db *store.DB, home string, project store.Project, task store.Task, old herdr.Pane, kind string) (herdr.Pane, error) {
	opened, err := s.openWorkerTab(ctx, db, home, project, task, task.WorktreePath, kind)
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
