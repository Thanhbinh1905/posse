package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) configSchema(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("config schema takes no arguments")
	}
	rows := make([]map[string]any, 0, len(config.Schema()))
	for _, spec := range config.Schema() {
		rows = append(rows, map[string]any{
			"key": spec.Key, "type": string(spec.Type), "allowed_values": spec.Allowed,
			"default": spec.Default, "meaning": spec.Meaning, "user_only": spec.UserOnly,
		})
	}
	return ctx.Print(axi.Object{{Key: "keys", Value: rows}, {Key: "help", Value: []any{"Run `posse config show --effective` to inspect effective values", "Run `posse config set <key> <value>` to update a key"}}})
}

func (s *Service) configShow(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("config show", args, map[string]flagSpec{"project": {}, "effective": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("config show takes no positional arguments")
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
	cfg, err := config.Load(home, projectName)
	if err != nil {
		return configError(err)
	}
	if parsed.Bool("effective") {
		sources, err := cfg.EffectiveSources()
		if err != nil {
			return configError(err)
		}
		result := axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "effective", Value: cfg}, {Key: "sources", Value: sources}}
		if len(cfg.Warnings) > 0 {
			result = append(result, axi.Field{Key: "warnings", Value: stringsToAny(cfg.Warnings)})
		}
		return ctx.Print(append(result, axi.Field{Key: "help", Value: []any{"Run `posse config schema` to inspect every key"}}))
	}
	path := config.ConfigPath(home, projectName)
	values, _, err := config.ReadFile(path)
	if err != nil {
		return configError(err)
	}
	result := axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "file", Value: path}, {Key: "values", Value: values}}
	if len(cfg.Warnings) > 0 {
		result = append(result, axi.Field{Key: "warnings", Value: stringsToAny(cfg.Warnings)})
	}
	return ctx.Print(append(result, axi.Field{Key: "help", Value: []any{"Run `posse config show --effective` to include defaults and Project overrides"}}))
}

func (s *Service) configSet(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("config set", args, map[string]flagSpec{"project": {}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 2 {
		return axi.Usage("config set requires <key> <value>")
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
	key := parsed.Positionals[0]
	if config.IsRemovedGateKey(key) {
		return configError(&config.InvalidError{Key: key, Reason: "command Gates were removed; PR delivery uses forge CI and local delivery has no automated verification"})
	}
	approvalProjectID, err := s.configApprovalProject(ctx.Context, db, key, projectName, parsed.Flags["user-approved"])
	if err != nil {
		return err
	}
	path := config.ConfigPath(home, projectName)
	original, originalMode, originalExists, err := configSnapshot(path)
	if err != nil {
		return err
	}
	priorIssues := config.ValidationIssues(home, projectName)
	value, err := config.SetFile(path, key, parsed.Positionals[1], projectName != "")
	if err != nil {
		return configError(err)
	}
	if err := s.validateConfigEdit(home, projectName, priorIssues); err != nil {
		if rollbackErr := restoreConfigFile(path, original, originalExists, originalMode); rollbackErr != nil {
			return errors.Join(configError(err), fmt.Errorf("restore previous config: %w", rollbackErr))
		}
		return configError(err)
	}
	if approvalProjectID != 0 {
		if err := db.RecordConfigApproval(ctx.Context, approvalProjectID, key, "set", parsed.Positionals[1], parsed.Flags["user-approved"]); err != nil {
			if rollbackErr := restoreConfigFile(path, original, originalExists, originalMode); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous config: %w", rollbackErr))
			}
			return err
		}
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	result := axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "setting", Value: key}, {Key: "value", Value: value}}
	result = append(result, axi.Field{Key: "help", Value: []any{"Run `posse config show --effective` to inspect merged settings"}})
	return ctx.Print(result)
}

func (s *Service) configUnset(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("config unset", args, map[string]flagSpec{"project": {}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("config unset requires <key>")
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
	key := parsed.Positionals[0]
	if config.IsRemovedGateKey(key) {
		return configError(&config.InvalidError{Key: key, Reason: "command Gates were removed; PR delivery uses forge CI and local delivery has no automated verification"})
	}
	approvalProjectID, err := s.configApprovalProject(ctx.Context, db, key, projectName, parsed.Flags["user-approved"])
	if err != nil {
		return err
	}
	path := config.ConfigPath(home, projectName)
	original, originalMode, originalExists, err := configSnapshot(path)
	if err != nil {
		return err
	}
	priorIssues := config.ValidationIssues(home, projectName)
	if err := config.UnsetFile(path, key, projectName != ""); err != nil {
		return configError(err)
	}
	if err := s.validateConfigEdit(home, projectName, priorIssues); err != nil {
		if rollbackErr := restoreConfigFile(path, original, originalExists, originalMode); rollbackErr != nil {
			return errors.Join(configError(err), fmt.Errorf("restore previous config: %w", rollbackErr))
		}
		return configError(err)
	}
	if approvalProjectID != 0 {
		if err := db.RecordConfigApproval(ctx.Context, approvalProjectID, key, "unset", "", parsed.Flags["user-approved"]); err != nil {
			if rollbackErr := restoreConfigFile(path, original, originalExists, originalMode); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous config: %w", rollbackErr))
			}
			return err
		}
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "unset", Value: key}, {Key: "help", Value: []any{"Run `posse config show --effective` to inspect merged settings"}}})
}

// configApprovalProject returns zero when no approval record is needed. Global
// changes are attributed to the calling Lead's Project.
func (s *Service) configApprovalProject(ctx context.Context, db *store.DB, key, projectName, quote string) (int64, error) {
	if !config.IsUserOnlyFor(key, projectName != "") {
		return 0, nil
	}
	return s.requireUserApproval(ctx, db, key, projectName, quote)
}

// requireUserApproval lets the User through and a Lead with the User's quote,
// returning the Project the approval is recorded under.
func (s *Service) requireUserApproval(ctx context.Context, db *store.DB, key, projectName, quote string) (int64, error) {
	actor := s.configCaller(ctx, db)
	role := actor.role
	if role == "user" {
		return 0, nil
	}
	if role != "lead" || strings.TrimSpace(quote) == "" {
		return 0, axi.Failure("user_only", key+" requires the User's approval", false, "Pass `--user-approved \"<User's words>\"` from the Lead pane or run this command from the User's shell")
	}
	if projectName != "" {
		project, err := s.projectByName(ctx, db, projectName)
		if err != nil {
			return 0, err
		}
		return project.ID, nil
	}
	return actor.projectID, nil
}

func (s *Service) validateConfigForEdit(home, projectName string) error {
	_, err := config.Load(home, projectName)
	return err
}

func (s *Service) validateConfigEdit(home, projectName string, priorIssues []error) error {
	previous := make(map[string]struct{}, len(priorIssues))
	for _, issue := range priorIssues {
		previous[issue.Error()] = struct{}{}
	}
	for _, issue := range config.ValidationIssues(home, projectName) {
		if _, alreadyPresent := previous[issue.Error()]; !alreadyPresent {
			return issue
		}
	}
	return nil
}

func (s *Service) requireConfigCaller(ctx context.Context, db *store.DB) error {
	if s.configCallerRole(ctx, db) == "worker" {
		return axi.Failure("lead_only", "this command is reserved for the Lead or User", false)
	}
	return nil
}

func (s *Service) configCallerRole(ctx context.Context, db *store.DB) string {
	return s.configCaller(ctx, db).role
}

func configSnapshot(path string) ([]byte, os.FileMode, bool, error) {
	original, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, false, err
	}
	return original, info.Mode().Perm(), true, nil
}

func restoreConfigFile(path string, original []byte, existed bool, mode os.FileMode) error {
	if existed {
		return atomicfile.Write(path, original, mode)
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func configScope(project string) string {
	if project == "" {
		return "global"
	}
	return "project:" + strings.TrimSpace(project)
}
