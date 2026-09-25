package app

import (
	"errors"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// lowkey uses the current Project when available, otherwise the global config.
// It is a reporting preference and does not change the Lead's authority.
func (s *Service) lowkey(ctx *axi.Context, args []string) error {
	if len(args) != 1 || !oneOfString(args[0], "on", "off", "status") {
		return axi.Usage("lowkey requires on, off or status")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	name := ""
	if root, rootErr := gitTop(ctx.Context, ""); rootErr == nil {
		if project, projectErr := db.ProjectByRoot(ctx.Context, root); projectErr == nil {
			name = project.Name
		} else if !store.IsNotFound(projectErr) {
			return projectErr
		} else if project, projectErr := s.projectForCWD(ctx.Context, db); projectErr == nil {
			name = project.Name
		}
	}
	if args[0] != "status" {
		path := config.ConfigPath(home, name)
		previous, mode, existed, err := configSnapshot(path)
		if err != nil {
			return err
		}
		if _, err := config.SetFile(path, "lowkey.lead", map[bool]string{true: "true", false: "false"}[args[0] == "on"], name != ""); err != nil {
			return configError(err)
		}
		if err := s.validateConfigForEdit(home, name); err != nil {
			return errors.Join(configError(err), restoreConfigFile(path, previous, existed, mode))
		}
		if err := s.regenerateProjects(ctx.Context, db); err != nil {
			return err
		}
	}
	cfg, err := config.Load(home, name)
	if err != nil {
		return configError(err)
	}
	state := "off"
	if cfg.Lowkey.Lead {
		state = "on"
	}
	return ctx.Print(axi.Object{{Key: "scope", Value: configScope(name)}, {Key: "lowkey", Value: state}, {Key: "help", Value: []any{"Run `posse lowkey on|off|status` to change or inspect lowkey mode"}}})
}
