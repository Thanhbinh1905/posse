package app

import (
	"context"
	"log"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func riderTabLabel(name string, last bool) string {
	branch := "├─"
	if last {
		branch = "└─"
	}
	return branch + " " + name
}

// relabelProjectTabs restores sidebar placement, plain tab labels and row
// metadata. Presentation failures never block the command.
func (s *Service) relabelProjectTabs(ctx context.Context, db *store.DB, project store.Project) {
	if s.Herdr == nil {
		return
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		log.Printf("posse: could not reconcile tab labels for Project %s: snapshot: %v", project.Name, err)
		return
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		log.Printf("posse: could not reconcile tab labels for Project %s: tasks: %v", project.Name, err)
		return
	}
	s.groupRiderWorkspaces(ctx, snapshot, project, tasks)
	labels, rows := projectTabPresentation(snapshot, project, tasks)
	for _, tab := range snapshot.Tabs {
		label, owned := labels[tab.TabID]
		if !owned || tab.Label == label {
			continue
		}
		if _, err := s.herdrCall(ctx, "tab.rename", map[string]any{"tab_id": tab.TabID, "label": label}); err != nil {
			log.Printf("posse: could not label Herdr tab %s as %q: %v", tab.TabID, label, err)
		}
	}
	for _, pane := range snapshot.Panes {
		row, owned := rows[pane.PaneID]
		if !owned {
			continue
		}
		metadata := map[string]any{"pane_id": pane.PaneID, "source": "posse", "tokens": map[string]string{"posse_row": row}}
		refreshLead := row == leadWorkspaceLabel(project) && pane.Agent != "" && pane.DisplayAgent != pane.Agent
		if pane.Tokens["posse_row"] == row && !refreshLead {
			continue
		}
		if refreshLead {
			metadata = leadDisplayMetadata(project, pane.PaneID, pane.Agent, "")
		}
		if _, err := s.herdrCall(ctx, "pane.report_metadata", metadata); err != nil {
			log.Printf("posse: could not label Herdr Agents row for pane %s: %v", pane.PaneID, err)
		}
	}
}

// projectTabPresentation only claims tabs identified by a Project Lead pane or a
// Task's labeled pane. Rider order follows the tab array from the snapshot.
func projectTabPresentation(snapshot herdr.Snapshot, project store.Project, tasks []store.Task) (map[string]string, map[string]string) {
	labels := make(map[string]string)
	rows := make(map[string]string)
	leadLabel := project.LeadLabel
	if leadLabel == "" {
		leadLabel = "posse:" + project.Name + ":lead"
	}
	lead, found := findAppPane(snapshot.Panes, project.LeadPaneID, leadLabel)
	leadTabID := ""
	if found && lead.TabID != "" {
		leadTabID = lead.TabID
		labels[leadTabID] = "Lead"
		rows[lead.PaneID] = leadWorkspaceLabel(project)
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

	var riderTabs []struct {
		tab  herdr.Tab
		name string
	}
	for _, tab := range snapshot.Tabs {
		candidate, exists := owners[tab.TabID]
		if !exists || candidate.ambiguous {
			continue
		}
		// Native child workspaces already render their workspace label. Keep
		// their tab label at Herdr's default instead of duplicating the name.
		linked := false
		for _, workspace := range snapshot.Workspaces {
			if workspace.WorkspaceID == tab.WorkspaceID && workspace.Worktree.CheckoutPath == candidate.task.WorktreePath {
				linked = true
				break
			}
		}
		if linked {
			if pane, found := findTaskPane(snapshot.Panes, candidate.task); found && pane.TabID == tab.TabID {
				rows[pane.PaneID] = ""
			}
			continue
		}
		riderTabs = append(riderTabs, struct {
			tab  herdr.Tab
			name string
		}{tab: tab, name: taskDisplayName(candidate.task)})
	}
	for index, rider := range riderTabs {
		labels[rider.tab.TabID] = rider.name
		if pane, found := findTaskPane(snapshot.Panes, owners[rider.tab.TabID].task); found && pane.TabID == rider.tab.TabID {
			rows[pane.PaneID] = riderTabLabel(rider.name, index == len(riderTabs)-1)
		}
	}
	return labels, rows
}
