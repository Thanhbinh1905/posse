package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) configSchema(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("config schema", args, map[string]flagSpec{"all": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("config schema takes no positional arguments")
	}
	schema := config.Schema()
	if parsed.Bool("all") {
		schema = config.SchemaAll()
	}
	rows := make([]map[string]any, 0, len(schema))
	for _, spec := range schema {
		rows = append(rows, map[string]any{
			"key": spec.Key, "type": string(spec.Type), "allowed_values": spec.Allowed,
			"default": spec.Default, "meaning": spec.Meaning, "user_only": spec.UserOnly,
		})
	}
	help := []any{"Run `posse config show --effective` to inspect effective values", "Run `posse config set <key> <value>` to update a key"}
	if !parsed.Bool("all") {
		help = append(help, "Run `posse config schema --all` to include advanced settings")
	}
	return ctx.Print(axi.Object{{Key: "keys", Value: rows}, {Key: "help", Value: help}})
}

func (s *Service) configShow(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("config show", args, map[string]flagSpec{"project": {}, "effective": {boolean: true}, "all": {boolean: true}})
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
		delete(sources, "identity.worker.display_prefix")
		effective := any(cfg)
		if !parsed.Bool("all") {
			encoded, err := json.Marshal(cfg)
			if err != nil {
				return err
			}
			var values map[string]any
			if err := json.Unmarshal(encoded, &values); err != nil {
				return err
			}
			hideAdvancedConfig(values)
			effective = values
			for key := range sources {
				if config.IsHiddenKey(key) {
					delete(sources, key)
				}
			}
		}
		result := axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "effective", Value: effective}, {Key: "sources", Value: sources}, {Key: "dispatch_rules", Value: effectiveDispatchRules(cfg)}, {Key: "dispatch_precedence", Value: stringsToAny(dispatch.Precedence)}}
		if len(cfg.Warnings) > 0 {
			result = append(result, axi.Field{Key: "warnings", Value: stringsToAny(cfg.Warnings)})
		}
		help := []any{"Run `posse config schema` to inspect User-facing keys"}
		if !parsed.Bool("all") {
			help = append(help, "Run `posse config show --effective --all` to include advanced settings")
		}
		return ctx.Print(append(result, axi.Field{Key: "help", Value: help}))
	}
	path := config.ConfigPath(home, projectName)
	values, _, err := config.ReadFile(path)
	if err != nil {
		return configError(err)
	}
	if identity, ok := values["identity"].(map[string]any); ok {
		if worker, ok := identity["worker"].(map[string]any); ok {
			delete(worker, "display_prefix")
			if len(worker) == 0 {
				delete(identity, "worker")
			}
		}
	}
	delete(values, "forge")
	if defaults, ok := values["defaults"].(map[string]any); ok {
		delete(defaults, "forge")
	}
	if !parsed.Bool("all") {
		hideAdvancedConfig(values)
	}
	result := axi.Object{{Key: "scope", Value: configScope(projectName)}, {Key: "file", Value: path}, {Key: "values", Value: values}}
	if len(cfg.Warnings) > 0 {
		result = append(result, axi.Field{Key: "warnings", Value: stringsToAny(cfg.Warnings)})
	}
	return ctx.Print(append(result, axi.Field{Key: "help", Value: []any{"Run `posse config show --effective` to include defaults and Project overrides", "Pass `--all` to include advanced settings"}}))
}

func hideAdvancedConfig(values map[string]any) {
	delete(values, "kinds")
	repositories, _ := values["repositories"].(map[string]any)
	for _, value := range repositories {
		repo, _ := value.(map[string]any)
		delete(repo, "forge")
	}
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
	if key == "up.projects" {
		if err := s.requireKnownProjects(ctx.Context, db, value); err != nil {
			if rollbackErr := restoreConfigFile(path, original, originalExists, originalMode); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous config: %w", rollbackErr))
			}
			return err
		}
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

// requireKnownProjects refuses an up.projects value naming an unregistered
// Project.
func (s *Service) requireKnownProjects(ctx context.Context, db *store.DB, value any) error {
	names, _ := value.([]any)
	for _, name := range names {
		if _, err := s.projectByName(ctx, db, fmt.Sprint(name)); err != nil {
			return err
		}
	}
	return nil
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

func effectiveDispatchRules(cfg config.Config) []any {
	rows := []any{}
	for _, rule := range cfg.Dispatch {
		rows = append(rows, axi.Object{
			{Key: "use", Value: strings.Join(rule.ProfileNames(), ", ")},
			{Key: "type", Value: defaultValue(rule.Type, "any")},
			{Key: "when", Value: rule.When},
			{Key: "source", Value: rule.Source},
			{Key: "precedence", Value: dispatch.RulePrecedence(rule)},
		})
	}
	if cfg.DispatchDefault.Use != "" {
		rows = append(rows, axi.Object{
			{Key: "use", Value: cfg.DispatchDefault.Use},
			{Key: "type", Value: "dispatch.default"},
			{Key: "when", Value: ""},
			{Key: "source", Value: cfg.DispatchDefault.Source},
			{Key: "precedence", Value: "default when no unconditional rule applies"},
		})
	}
	return rows
}
