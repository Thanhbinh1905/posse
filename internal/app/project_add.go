package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// projectAdd registers the current folder without a confirmation prompt: the
// command itself is the User's explicit request.
func (s *Service) projectAdd(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("project add", args, map[string]flagSpec{"name": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("project add takes no positional arguments")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.requireConfigCaller(ctx.Context, db); err != nil {
		return err
	}
	dir, err := currentDir()
	if err != nil {
		return err
	}
	if project, found, lookupErr := registeredProjectFor(ctx.Context, db, dir); lookupErr != nil {
		return lookupErr
	} else if found {
		return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "root", Value: project.Root}, {Key: "registered", Value: false}, {Key: "help", Value: []any{"Run `posse config set --project " + project.Name + " <key> <value>` to configure it"}}})
	}
	detected, err := detectProject(ctx.Context, dir)
	if err != nil {
		return err
	}
	if err := applyProjectName(&detected, parsed.Flags["name"]); err != nil {
		return err
	}
	if _, lookupErr := db.ProjectByName(ctx.Context, detected.Name); lookupErr == nil {
		return axi.Failure("name_taken", fmt.Sprintf("Project name %q is already registered", detected.Name), false, "Pass a different `--name <n>`")
	} else if !store.IsNotFound(lookupErr) {
		return lookupErr
	}
	cfg, err := config.Load(home, detected.Name)
	if err != nil {
		return configError(err)
	}
	project, err := registerDetected(ctx.Context, db, detected)
	if err != nil {
		return err
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	result := detectionObject(detected, cfg)
	result = append(axi.Object{{Key: "project", Value: project.Name}, {Key: "registered", Value: true}}, result...)
	result = append(result, axi.Field{Key: "help", Value: []any{"Run `posse config set --project " + project.Name + " <key> <value>` to configure it", "Run `posse up` to start its Lead"}})
	return ctx.Print(result)
}

func applyProjectName(detected *detectedProject, requested string) error {
	if requested != "" {
		detected.Name = projectName(requested)
	}
	if detected.Name == "" {
		return axi.Failure("project_name_required", "Project name cannot be empty", false, "Pass `--name <n>`")
	}
	return nil
}

// projectScan re-detects a workspace Project's members after repositories were
// added to or removed from its folder.
func (s *Service) projectScan(ctx *axi.Context, args []string) error {
	if len(args) > 1 {
		return axi.Usage("project scan takes at most one Project name")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.requireConfigCaller(ctx.Context, db); err != nil {
		return err
	}
	var project store.Project
	if len(args) == 1 {
		project, err = s.projectByName(ctx.Context, db, args[0])
	} else {
		project, err = s.projectForCWD(ctx.Context, db)
	}
	if err != nil {
		return err
	}
	if !project.IsWorkspace() {
		return axi.Failure("project_not_workspace", "Project "+project.Name+" is a single repository and has no members to scan", false, "Run `posse project show` to inspect it")
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	result, err := s.rescanWorkspace(ctx.Context, db, project)
	if err != nil {
		return err
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	repos, err := s.projectRepoRows(ctx.Context, db, project, cfg)
	if err != nil {
		return err
	}
	return ctx.Print(axi.Object{
		{Key: "project", Value: project.Name}, {Key: "added", Value: result.Added}, {Key: "missing", Value: result.Missing},
		{Key: "repos", Value: repos},
		{Key: "help", Value: []any{"Name members in a Brief's `repos:` field to work on them"}},
	})
}

type rescanResult struct {
	Added   []string
	Missing []string
}

func (s *Service) rescanWorkspace(ctx context.Context, db *store.DB, project store.Project) (rescanResult, error) {
	result := rescanResult{Added: []string{}, Missing: []string{}}
	scanned, err := scanWorkspace(ctx, project.Root)
	if err != nil {
		return result, err
	}
	known, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return result, err
	}
	active := map[string]bool{}
	for _, repo := range known {
		active[repo.Name] = repo.Status == store.RepoActive
	}
	seen := map[string]bool{}
	rows := make([]store.ProjectRepo, 0, len(scanned))
	for _, repo := range scanned {
		seen[repo.Name] = true
		if !active[repo.Name] {
			result.Added = append(result.Added, repo.Name)
		}
		rows = append(rows, store.ProjectRepo{Name: repo.Name, Path: repo.Path, DefaultBranch: repo.DefaultBranch})
	}
	for name, isActive := range active {
		if isActive && !seen[name] {
			result.Missing = append(result.Missing, name)
		}
	}
	return result, db.SyncProjectRepos(ctx, project.ID, rows)
}

// projectRepoRows describes each member for `posse project show` and `scan`.
func (s *Service) projectRepoRows(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) ([]any, error) {
	repos, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(repos))
	for _, repo := range repos {
		root := filepath.Join(project.Root, repo.Path)
		remote := originHost(ctx, root)
		shown := remote
		if shown == "" {
			shown = "none"
		}
		rows = append(rows, axi.Object{
			{Key: "name", Value: repo.Name}, {Key: "default_branch", Value: repo.DefaultBranch},
			{Key: "remote", Value: shown}, {Key: "landing_mode", Value: memberLandingMode(cfg, repo.Name, remote)},
			{Key: "status", Value: repo.Status},
		})
	}
	return rows, nil
}
