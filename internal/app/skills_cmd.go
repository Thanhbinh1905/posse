package app

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/term"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	setupassets "github.com/thanhbinh1905/posse/internal/setup"
)

func (s *Service) skillsShow(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("skills", args, map[string]flagSpec{"project": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("skills takes no positional arguments; use `skills add` to install skills")
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
	}
	if err := s.validateConfigForEdit(home, projectName); err != nil {
		return configError(err)
	}
	path := config.ConfigPath(home, projectName)
	configured, err := config.ConfiguredSkills(path)
	if err != nil {
		return configError(err)
	}
	missing := config.MissingRecommendedSkills(configured)
	rows := make([]map[string]any, 0, len(config.SkillRoles))
	for _, role := range config.SkillRoles {
		rows = append(rows, map[string]any{"role": role, "configured": nonNil(configured[role]), "recommended_missing": nonNil(missing[role])})
	}
	return ctx.Print(axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "file", Value: path}, {Key: "roles", Value: rows}, {Key: "help", Value: []any{"Run `posse skills add` to pick recommended skills, or `posse skills add --all` to install them all", "Edit the [skills] block in the file to remove skills or add your own directories"}}})
}

func (s *Service) skillsAdd(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("skills add", args, map[string]flagSpec{"all": {boolean: true}, "project": {}, "user-approved": {}})
	if err != nil {
		return err
	}
	all := parsed.Bool("all")
	if all && len(parsed.Positionals) != 0 {
		return axi.Usage("skills add takes --all or skill names, not both")
	}
	recommended := setupassets.RecommendedSkills()
	for _, name := range parsed.Positionals {
		if !recommendedSkill(recommended, name) {
			return axi.Usage(fmt.Sprintf("%q is not a recommended skill", name), "Run `posse skills` to list the recommended skills", "Write your own skill directories into the [skills] block of the config file")
		}
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	if !all && len(parsed.Positionals) == 0 && !interactive {
		return axi.Usage("skills add needs --all or skill names without a terminal", "Run `posse skills add --all` to install the recommended set", "Run `posse skills add <name>...` to install some of it")
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
	}
	approvalProjectID, err := s.requireUserApproval(ctx.Context, db, "skills", projectName, parsed.Flags["user-approved"])
	if err != nil {
		return err
	}
	if err := s.validateConfigForEdit(home, projectName); err != nil {
		return configError(err)
	}
	path := config.ConfigPath(home, projectName)
	configured, err := config.ConfiguredSkills(path)
	if err != nil {
		return configError(err)
	}
	selection := map[string][]string{}
	switch {
	case all:
		selection = recommended
	case len(parsed.Positionals) > 0:
		for role, names := range recommended {
			for _, name := range names {
				if slices.Contains(parsed.Positionals, name) {
					selection[role] = append(selection[role], name)
				}
			}
		}
	default:
		picked, saved, err := pickRecommendedSkills(recommended, configured, displayPath(path))
		if err != nil {
			return err
		}
		if !saved {
			return ctx.Print(axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "file", Value: path}, {Key: "added", Value: []any{}}, {Key: "help", Value: []any{"Nothing was written"}}})
		}
		selection = picked
	}
	added, err := config.AddSkills(path, selection)
	if err != nil {
		return configError(err)
	}
	rows := []map[string]any{}
	for _, role := range config.SkillRoles {
		if len(added[role]) == 0 {
			continue
		}
		rows = append(rows, map[string]any{"role": role, "skills": added[role]})
		if approvalProjectID != 0 {
			if err := db.RecordConfigApproval(ctx.Context, approvalProjectID, "skills."+role, "set", strings.Join(added[role], ","), parsed.Flags["user-approved"]); err != nil {
				return err
			}
		}
	}
	if len(rows) > 0 {
		if err := s.regenerateProjects(ctx.Context, db); err != nil {
			return err
		}
	}
	return ctx.Print(axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "file", Value: path}, {Key: "added", Value: rows}, {Key: "help", Value: []any{"New Tasks and Leads load the added skills; running ones keep theirs", "Run `posse skills` to inspect the result"}}})
}

func recommendedSkill(recommended map[string][]string, name string) bool {
	for _, names := range recommended {
		if slices.Contains(names, name) {
			return true
		}
	}
	return false
}

func nonNil(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

func pickRecommendedSkills(recommended, configured map[string][]string, target string) (map[string][]string, bool, error) {
	picker := skillPicker{target: target}
	for _, role := range config.SkillRoles {
		for _, name := range recommended[role] {
			picker.choices = append(picker.choices, skillChoice{role: role, name: name, summary: setupassets.SkillSummary(name), configured: slices.Contains(configured[role], name)})
		}
	}
	picker.width, picker.height, _ = term.GetSize(int(os.Stdout.Fd()))
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, false, err
	}
	defer term.Restore(int(os.Stdin.Fd()), state)
	choices, saved, err := runSkillPicker(os.Stdin, os.Stdout, picker)
	if err != nil || !saved {
		return nil, false, err
	}
	selection := map[string][]string{}
	for _, choice := range choices {
		if choice.selected {
			selection[choice.role] = append(selection[choice.role], choice.name)
		}
	}
	return selection, true, nil
}
