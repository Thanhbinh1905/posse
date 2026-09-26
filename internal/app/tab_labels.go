package app

import (
	"context"
	"log"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

const leadTabLabel = "Lead"

func riderTabLabel(name string, last bool) string {
	branch := "├─"
	if last {
		branch = "└─"
	}
	return branch + " " + name
}

// relabelProjectTabs restores Posse's tab labels from the current Herdr order.
// A rename is presentation-only: failures are logged and never block the
// command that triggered reconciliation.
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
	labels := projectTabLabels(snapshot, project, tasks)
	for _, tab := range snapshot.Tabs {
		label, owned := labels[tab.TabID]
		if !owned || tab.Label == label {
			continue
		}
		if _, err := s.herdrCall(ctx, "tab.rename", map[string]any{"tab_id": tab.TabID, "label": label}); err != nil {
			log.Printf("posse: could not label Herdr tab %s as %q: %v", tab.TabID, label, err)
		}
	}
}

// projectTabLabels only claims tabs identified by a Project Lead pane or a
// Task's labeled pane. Rider order follows the tab array from the snapshot.
func projectTabLabels(snapshot herdr.Snapshot, project store.Project, tasks []store.Task) map[string]string {
	labels := make(map[string]string)
	leadLabel := project.LeadLabel
	if leadLabel == "" {
		leadLabel = "posse:" + project.Name + ":lead"
	}
	lead, found := findAppPane(snapshot.Panes, project.LeadPaneID, leadLabel)
	leadTabID := ""
	if found && lead.TabID != "" {
		leadTabID = lead.TabID
		labels[leadTabID] = leadTabLabel
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
		riderTabs = append(riderTabs, struct {
			tab  herdr.Tab
			name string
		}{tab: tab, name: taskDisplayName(candidate.task)})
	}
	for index, rider := range riderTabs {
		labels[rider.tab.TabID] = riderTabLabel(rider.name, index == len(riderTabs)-1)
	}
	return labels
}
