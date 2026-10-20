package app

import (
	"fmt"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
)

func (s *Service) workflowShow(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("workflow show", args, map[string]flagSpec{"project": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("workflow show requires <name>")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	projectName := parsed.Flags["project"]
	if projectName != "" {
		if _, err := s.projectByName(ctx.Context, db, projectName); err != nil {
			return err
		}
	} else {
		dir, err := currentDir()
		if err != nil {
			return err
		}
		project, found, err := registeredProjectFor(ctx.Context, db, dir)
		if err != nil {
			return err
		}
		if found {
			projectName = project.Name
		}
	}
	cfg, err := config.Load(home, projectName)
	if err != nil {
		return configError(err)
	}
	workflow, found, err := cfg.ResolveWorkflow(parsed.Positionals[0])
	if err != nil {
		return err
	}
	if !found {
		return unknownWorkflow(parsed.Positionals[0], projectName)
	}
	return ctx.Print(axi.Object{
		{Key: "name", Value: workflow.Name},
		{Key: "source", Value: workflow.Source},
		{Key: "sha256", Value: workflow.SHA256},
		{Key: "profile", Value: workflow.Profile},
		{Key: "help", Value: []any{"Select this profile with `workflow: " + workflow.Name + "` in a Task Brief", "Stage transitions remain User-decided"}},
	})
}

func unknownWorkflow(name, projectName string) error {
	scope := "User config"
	if projectName != "" {
		scope += " or Project " + projectName + " config"
	}
	return axi.Failure("workflow_unknown", fmt.Sprintf("unknown workflow profile %q (workflows.%s) in %s", name, name, scope), false, "Define the named profile under [workflows."+name+"]")
}
