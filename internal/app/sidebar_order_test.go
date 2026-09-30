package app

import (
	"bytes"
	"context"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestRiderWorkspacePlacement(t *testing.T) {
	project := store.Project{Name: "shop", HerdrWorkspaceID: "lead", LeadPaneID: "lead:p1", LeadLabel: "posse:shop:lead"}
	tasks := []store.Task{
		{PaneID: "first:p1", PaneLabel: "posse:shop:t1", WorktreePath: "/mount-1"},
		{PaneID: "second:p1", PaneLabel: "posse:shop:t2", WorktreePath: "/mount-2"},
	}
	workspace := func(id, path string, linked bool) herdr.Workspace {
		w := herdr.Workspace{WorkspaceID: id, Label: id}
		w.Worktree.CheckoutPath, w.Worktree.RepoKey, w.Worktree.IsLinkedWorktree = path, "repo", linked
		return w
	}
	for _, tt := range []struct {
		name, before string
		order, want  []string
		mutate       func(*herdr.Snapshot, *store.Project)
	}{
		{name: "appended children", order: []string{"notes", "lead", "scratch", "first", "second"}, want: []string{"first", "second"}, before: "scratch"},
		{name: "already adjacent", order: []string{"notes", "lead", "first", "second", "scratch"}},
		{name: "interleaved children", order: []string{"lead", "second", "scratch", "first"}, want: []string{"second", "first"}, before: "scratch"},
		{name: "children before Lead", order: []string{"first", "notes", "second", "lead"}, want: []string{"first", "second"}},
		{name: "missing Lead", order: []string{"notes", "first", "second"}, mutate: func(s *herdr.Snapshot, _ *store.Project) { s.Panes = s.Panes[1:] }},
		{name: "reused Lead id", order: []string{"lead", "scratch", "first", "second"}, mutate: func(s *herdr.Snapshot, _ *store.Project) { s.Panes[0].Label = "foreign" }},
		{name: "reused Rider ids", order: []string{"lead", "scratch", "first", "second"}, mutate: func(s *herdr.Snapshot, _ *store.Project) {
			s.Panes[1].Label, s.Panes[2].Label = "foreign", "foreign"
		}},
		{name: "foreign provenance", order: []string{"lead", "scratch", "first", "second"}, mutate: func(s *herdr.Snapshot, _ *store.Project) {
			s.Workspaces[2].Worktree.RepoKey = "another-repo"
			s.Workspaces[3].Worktree.CheckoutPath = "/another-mount"
		}},
		{name: "legacy standalone Riders", order: []string{"lead", "scratch", "first", "second"}, mutate: func(s *herdr.Snapshot, _ *store.Project) {
			s.Workspaces[2].Worktree.IsLinkedWorktree, s.Workspaces[3].Worktree.IsLinkedWorktree = false, false
		}},
		{name: "Workspace Project", order: []string{"lead", "scratch", "first", "second"}, mutate: func(_ *herdr.Snapshot, p *store.Project) { p.Kind = store.ProjectKindWorkspace }},
		{name: "Lead followed by its label", order: []string{"notes", "lead", "scratch", "first", "second"}, want: []string{"first", "second"}, before: "scratch", mutate: func(_ *herdr.Snapshot, p *store.Project) {
			p.HerdrWorkspaceID, p.LeadPaneID = "old-lead", "old-lead:p1"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := project
			snap := herdr.Snapshot{Panes: []herdr.Pane{
				{PaneID: "lead:p1", WorkspaceID: "lead", Label: p.LeadLabel},
				{PaneID: tasks[0].PaneID, WorkspaceID: "first", Label: tasks[0].PaneLabel},
				{PaneID: tasks[1].PaneID, WorkspaceID: "second", Label: tasks[1].PaneLabel},
			}}
			for _, id := range tt.order {
				path := map[string]string{"first": "/mount-1", "second": "/mount-2"}[id]
				snap.Workspaces = append(snap.Workspaces, workspace(id, path, path != ""))
			}
			if tt.mutate != nil {
				tt.mutate(&snap, &p)
			}
			ids, before := riderWorkspacePlacement(snap, p, tasks)
			if !slices.Equal(ids, tt.want) || before != tt.before {
				t.Fatalf("placement = %v before %q, want %v before %q", ids, before, tt.want, tt.before)
			}
		})
	}
}

func TestGroupRiderWorkspacesLogsMoveFailure(t *testing.T) {
	fake := herdr.NewFake()
	fake.Errors["workspace.move_block"] = &herdr.Error{Code: "workspace_not_found", Message: "child closed"}
	project := store.Project{Name: "shop", LeadLabel: "posse:shop:lead"}
	lead := herdr.Workspace{WorkspaceID: "lead"}
	lead.Worktree.RepoKey = "repo"
	rider := herdr.Workspace{WorkspaceID: "rider"}
	rider.Worktree.RepoKey, rider.Worktree.CheckoutPath, rider.Worktree.IsLinkedWorktree = "repo", "/mount", true
	snap := herdr.Snapshot{
		Workspaces: []herdr.Workspace{lead, {WorkspaceID: "user"}, rider},
		Panes: []herdr.Pane{
			{PaneID: "lead:p1", WorkspaceID: "lead", Label: project.LeadLabel},
			{PaneID: "rider:p1", WorkspaceID: "rider", Label: "posse:shop:t1"},
		},
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	testService(t.TempDir(), fake).groupRiderWorkspaces(context.Background(), snap, project, []store.Task{{PaneLabel: "posse:shop:t1", WorktreePath: "/mount"}})
	if fake.CallCount("workspace.move_block") != 1 || !strings.Contains(output.String(), "could not group Rider workspaces under Lead") {
		t.Fatalf("move failure not reported: calls=%#v log=%s", fake.Calls, output.String())
	}
	params := fake.Calls[0].Params
	if !slices.Equal(params["workspace_ids"].([]string), []string{"rider"}) || params["before_workspace_id"] != "user" {
		t.Fatalf("move included a foreign workspace: %#v", params)
	}
}
