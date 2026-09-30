package app

import (
	"context"
	"log"
	"slices"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// Herdr's Agents panel follows workspace order, not worktree provenance. Move
// only identified linked Riders, preserving their order and all other labels.
func (s *Service) groupRiderWorkspaces(ctx context.Context, snapshot herdr.Snapshot, project store.Project, tasks []store.Task) {
	ids, before := riderWorkspacePlacement(snapshot, project, tasks)
	if len(ids) == 0 {
		return
	}
	params := map[string]any{"workspace_ids": ids}
	if before != "" {
		params["before_workspace_id"] = before
	}
	if _, err := s.herdrCall(ctx, "workspace.move_block", params); err != nil {
		log.Printf("posse: could not group Rider workspaces under Lead for Project %s: %v", project.Name, err)
	}
}

// Return the Rider block and its next non-Rider anchor only if a move is needed.
// Labels and checkout provenance, never recorded ids alone, establish ownership.
func riderWorkspacePlacement(snapshot herdr.Snapshot, project store.Project, tasks []store.Task) ([]string, string) {
	if project.IsWorkspace() {
		return nil, ""
	}
	leadID, found := leadWorkspace(snapshot, project)
	if !found {
		return nil, ""
	}
	leadIndex := -1
	leadRepoKey := ""
	for i, workspace := range snapshot.Workspaces {
		if workspace.WorkspaceID == leadID {
			leadIndex, leadRepoKey = i, workspace.Worktree.RepoKey
			break
		}
	}
	if leadIndex < 0 || leadRepoKey == "" {
		return nil, ""
	}
	owned := make(map[string]bool)
	for _, task := range tasks {
		pane, found := findTaskPane(snapshot.Panes, task)
		if !found || task.PaneLabel == "" || pane.Label != task.PaneLabel || pane.WorkspaceID == leadID {
			continue
		}
		for _, workspace := range snapshot.Workspaces {
			if workspace.WorkspaceID == pane.WorkspaceID && workspace.Worktree.IsLinkedWorktree &&
				workspace.Worktree.RepoKey == leadRepoKey && workspace.Worktree.CheckoutPath == task.WorktreePath {
				owned[workspace.WorkspaceID] = true
				break
			}
		}
	}
	var ids []string
	for _, workspace := range snapshot.Workspaces {
		if owned[workspace.WorkspaceID] {
			ids = append(ids, workspace.WorkspaceID)
		}
	}
	if len(ids) == 0 {
		return nil, ""
	}
	var following []string
	before := ""
	for _, workspace := range snapshot.Workspaces[leadIndex+1:] {
		if !owned[workspace.WorkspaceID] {
			before = workspace.WorkspaceID
			break
		}
		following = append(following, workspace.WorkspaceID)
	}
	if slices.Equal(ids, following) {
		return nil, ""
	}
	return ids, before
}
