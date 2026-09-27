package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// projectRemove unregisters a Project that never had a Task or a Mount, such as
// one registered by mistake. It is a dry run without --yes.
func (s *Service) projectRemove(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("project remove", args, map[string]flagSpec{"yes": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("project remove requires <name>", "Run `posse` outside a Project to list registered Projects")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.requireConfigCaller(ctx.Context, db); err != nil {
		return err
	}
	project, err := s.projectByName(ctx.Context, db, parsed.Positionals[0])
	if err != nil {
		return err
	}
	tasks, mounts, err := db.ProjectContents(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	if tasks != 0 || mounts != 0 {
		return projectNotEmpty(project, tasks, mounts)
	}
	if s.Herdr != nil && (project.LeadPaneID != "" || project.LeadLabel != "") {
		snapshot, snapshotErr := s.Herdr.Snapshot(ctx.Context)
		if snapshotErr != nil && !isHerdrUnavailable(snapshotErr) {
			return herdrError(snapshotErr)
		}
		for _, pane := range snapshot.Panes {
			if pane.Agent != "" && ((project.LeadPaneID != "" && pane.PaneID == project.LeadPaneID) || (project.LeadLabel != "" && pane.Label == project.LeadLabel)) {
				return axi.Failure("lead_running", fmt.Sprintf("Project %s still has a live Lead in pane %s", project.Name, pane.PaneID), false, "Stop that Lead before removing its Project")
			}
		}
	}
	projectHome := filepath.Join(home, "projects", project.Name)
	if project.Name == "" || filepath.Base(project.Name) != project.Name || project.Name == "." || project.Name == ".." {
		return axi.Failure("project_name_invalid", fmt.Sprintf("Project name %q does not name a directory under %s", project.Name, filepath.Join(home, "projects")), false)
	}
	if !parsed.Bool("yes") {
		return ctx.Print(axi.Object{
			{Key: "project", Value: project.Name},
			{Key: "root", Value: project.Root},
			{Key: "home", Value: projectHome},
			{Key: "removed", Value: false},
			{Key: "help", Value: []any{"Run `posse project remove " + project.Name + " --yes` to unregister it and delete " + projectHome}},
		})
	}
	if err := db.DeleteEmptyProject(ctx.Context, project.ID); err != nil {
		if errors.Is(err, store.ErrProjectNotEmpty) {
			return projectNotEmpty(project, tasks, mounts)
		}
		if errors.Is(err, store.ErrProjectReferenced) {
			return axi.Failure("project_has_dependents", fmt.Sprintf("Project %s has related records that prevent removal", project.Name), false, "Inspect and remove related records before retrying")
		}
		return err
	}
	if err := os.RemoveAll(projectHome); err != nil {
		return axi.Failure("project_home_remove_failed", "Project was unregistered but its home directory could not be deleted", false, err.Error())
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "root", Value: project.Root},
		{Key: "removed", Value: true},
		{Key: "help", Value: []any{"The repository at " + project.Root + " was not touched"}},
	})
}

func projectNotEmpty(project store.Project, tasks, mounts int) error {
	return axi.Failure("project_not_empty",
		fmt.Sprintf("Project %s has %d Tasks and %d Mounts; only a Project without either can be removed", project.Name, tasks, mounts),
		false, "A Project with Task history stays registered; use `posse project move` if its repository moved")
}
