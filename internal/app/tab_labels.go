package app

import (
	"context"
	"log"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// relabelProjectTabs restores sidebar placement, plain tab labels and row
// metadata. Presentation failures never block the command.
func (s *Service) relabelProjectTabs(ctx context.Context, db *store.DB, project store.Project) {
	if s.Herdr == nil || ctx.Err() != nil {
		return
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("posse: could not reconcile tab labels for Project %s: snapshot: %v", project.Name, err)
		}
		return
	}
	// Only a Task whose pane label is live can own a tab.
	var paneLabels []string
	for _, pane := range snapshot.Panes {
		if pane.Label != "" {
			paneLabels = append(paneLabels, pane.Label)
		}
	}
	tasks, err := db.TasksWithPaneLabels(ctx, project.ID, paneLabels)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("posse: could not reconcile tab labels for Project %s: tasks: %v", project.Name, err)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	labels := projectTabPresentation(snapshot, project, tasks)
	for _, tab := range snapshot.Tabs {
		if ctx.Err() != nil {
			return
		}
		label, owned := labels[tab.TabID]
		if !owned || tab.Label == label {
			continue
		}
		if _, err := s.herdrCall(ctx, "tab.rename", map[string]any{"tab_id": tab.TabID, "label": label}); err != nil {
			if ctx.Err() == nil {
				log.Printf("posse: could not label Herdr tab %s as %q: %v", tab.TabID, label, err)
			}
			if ctx.Err() != nil {
				return
			}
		}
	}
	leadLabel := project.LeadLabel
	if leadLabel == "" {
		leadLabel = "posse:" + project.Name + ":lead"
	}
	lead, found := findAppPane(snapshot.Panes, project.LeadPaneID, leadLabel)
	if found && ctx.Err() == nil && lead.Agent != "" && lead.DisplayAgent != lead.Agent {
		if _, err := s.herdrCall(ctx, "pane.report_metadata", leadDisplayMetadata(project, lead.PaneID, lead.Agent, "")); err != nil && ctx.Err() == nil {
			log.Printf("posse: could not refresh Lead harness for pane %s: %v", lead.PaneID, err)
		}
	}
}

// projectTabPresentation claims tabs identified by the Lead pane or a Task's
// labeled pane. New Worker tabs get plain labels; legacy linked-child workspaces
// keep their native workspace labels.
func legacyLinkedWorkerTab(snapshot herdr.Snapshot, tab herdr.Tab, task store.Task) bool {
	for _, workspace := range snapshot.Workspaces {
		if workspace.WorkspaceID == tab.WorkspaceID && workspace.Worktree.IsLinkedWorktree && workspace.Worktree.CheckoutPath == task.WorktreePath {
			return true
		}
	}
	return false
}

func projectTabPresentation(snapshot herdr.Snapshot, project store.Project, tasks []store.Task) map[string]string {
	labels := make(map[string]string)
	leadLabel := project.LeadLabel
	if leadLabel == "" {
		leadLabel = "posse:" + project.Name + ":lead"
	}
	lead, found := findAppPane(snapshot.Panes, project.LeadPaneID, leadLabel)
	leadTabID := ""
	if found && lead.TabID != "" {
		leadTabID = lead.TabID
		labels[leadTabID] = "Lead"
	}

	type owner struct {
		task      store.Task
		ambiguous bool
	}
	owners := make(map[string]owner)
	for _, task := range tasks {
		for tabID := range taskTabs(snapshot, project, task) {
			if tabID == leadTabID {
				continue
			}
			previous, exists := owners[tabID]
			if exists && previous.task.ID != task.ID {
				previous.ambiguous = true
				owners[tabID] = previous
				continue
			}
			owners[tabID] = owner{task: task}
		}
	}

	var workerTabs []struct {
		tab  herdr.Tab
		name string
	}
	for _, tab := range snapshot.Tabs {
		candidate, exists := owners[tab.TabID]
		if !exists || candidate.ambiguous {
			continue
		}
		linked := legacyLinkedWorkerTab(snapshot, tab, candidate.task)
		if linked {
			continue
		}
		workerTabs = append(workerTabs, struct {
			tab  herdr.Tab
			name string
		}{tab: tab, name: workerTabLabel(candidate.task)})
	}
	for _, worker := range workerTabs {
		labels[worker.tab.TabID] = worker.name
	}
	return labels
}
