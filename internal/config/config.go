package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Identity struct {
	Lead   IdentityRole `toml:"lead" json:"lead"`
	Worker WorkerRole   `toml:"worker" json:"worker"`
}

type IdentityRole struct {
	Name        string `toml:"name" json:"name"`
	Persona     string `toml:"persona" json:"persona"`
	Language    string `toml:"language" json:"language"`
	AddressUser string `toml:"address_user" json:"address_user"`
}

type WorkerRole struct {
	DisplayPrefix string `toml:"display_prefix" json:"display_prefix"`
}

type Lead struct {
	Kind     string            `toml:"kind" json:"kind"`
	Profiles map[string]string `toml:"profiles" json:"profiles"`
}

type Defaults struct {
	MaxWorkers          int      `toml:"max_workers" json:"max_workers"`
	DispatchInheritance string   `toml:"dispatch_inheritance" json:"dispatch_inheritance"`
	StallAfter          string   `toml:"stall_after" json:"stall_after"`
	IdleAfter           string   `toml:"idle_after" json:"idle_after"`
	AutoTeardown        string   `toml:"auto_teardown" json:"auto_teardown"`
	AutoRecover         bool     `toml:"auto_recover" json:"auto_recover"`
	RecoveryAttempts    int      `toml:"recovery_attempts" json:"recovery_attempts"`
	RecoveryBackoff     string   `toml:"recovery_backoff" json:"recovery_backoff"`
	MergeMode           string   `toml:"merge_mode" json:"merge_mode"`
	TargetBranch        string   `toml:"target_branch" json:"target_branch"`
	MergeMethod         string   `toml:"merge_method" json:"merge_method"`
	Forge               string   `toml:"forge" json:"forge"`
	PRPoll              string   `toml:"pr_poll" json:"pr_poll"`
	Review              string   `toml:"review" json:"review"`
	Gate                []string `toml:"gate" json:"gate"`
}

type ReviewLoop struct {
	Enabled   bool     `toml:"enabled" json:"enabled"`
	MaxRounds int      `toml:"max_rounds" json:"max_rounds"`
	BlockAt   string   `toml:"block_at" json:"block_at"`
	AutoStart bool     `toml:"auto_start" json:"auto_start"`
	Docs      bool     `toml:"docs" json:"docs"`
	Guidance  []string `toml:"guidance" json:"guidance"`
}

type GateLoop struct {
	MaxFixRounds int `toml:"max_fix_rounds" json:"max_fix_rounds"`
}

type CILoop struct {
	MaxFixRounds   int `toml:"max_fix_rounds" json:"max_fix_rounds"`
	RerunCancelled int `toml:"rerun_cancelled" json:"rerun_cancelled"`
}

type Pool struct {
	Clean    string   `toml:"clean" json:"clean"`
	Setup    []string `toml:"setup" json:"setup"`
	KeepIdle int      `toml:"keep_idle" json:"keep_idle"`
}

type Retention struct {
	TaskArtifacts string `toml:"task_artifacts" json:"task_artifacts"`
}

type Kind struct {
	Disabled           bool     `toml:"disabled" json:"disabled"`
	AutoApproveArgs    []string `toml:"auto_approve_args" json:"auto_approve_args"`
	LeadAutoApprove    bool     `toml:"lead_auto_approve" json:"lead_auto_approve"`
	ModelArgs          []string `toml:"model_args" json:"model_args"`
	EffortArgs         []string `toml:"effort_args" json:"effort_args"`
	ResumeArgs         []string `toml:"resume_args" json:"resume_args"`
	SystemPromptArgs   []string `toml:"system_prompt_args" json:"system_prompt_args"`
	LeadArgs           []string `toml:"lead_args" json:"lead_args"`
	Prepare            string   `toml:"prepare" json:"prepare"`
	NoticeDelivery     string   `toml:"notice_delivery" json:"notice_delivery"`
	BackgroundCommands bool     `toml:"background_commands" json:"background_commands"`
	Steer              bool     `toml:"steer" json:"steer"`
	InterruptKeys      []string `toml:"interrupt_keys" json:"interrupt_keys"`
}

// Notice delivery modes: how Notices reach a Lead of this kind. An empty value
// means NoticeDeliveryPrompt.
const (
	NoticeDeliveryPrompt         = "prompt"          // type a prompt when the pane is idle and unfocused
	NoticeDeliveryWatch          = "watch"           // Claude owns a bounded Stop hook; other kinds use a background Watch
	NoticeDeliveryCodexQueue     = "codex-queue"     // posse queues the prompt with `codex queue`
	NoticeDeliveryPiExtension    = "pi-extension"    // a posse pi extension runs `posse watch`
	NoticeDeliveryOpenCodePlugin = "opencode-plugin" // a Lead-only plugin queues watch batches through the session API
)

var NoticeDeliveries = []string{NoticeDeliveryPrompt, NoticeDeliveryWatch, NoticeDeliveryCodexQueue, NoticeDeliveryPiExtension, NoticeDeliveryOpenCodePlugin}

type Profile struct {
	Kind   string   `toml:"kind" json:"kind"`
	Model  string   `toml:"model" json:"model"`
	Effort string   `toml:"effort" json:"effort"`
	Args   []string `toml:"args" json:"args"`
}

type Rule struct {
	Type string `toml:"type" json:"type"`
	When string `toml:"when" json:"when"`
	Use  string `toml:"use" json:"use"`
}

type Autonomy struct {
	Yolo   bool   `toml:"yolo" json:"yolo"`
	Review string `toml:"review" json:"review"`
	Merge  string `toml:"merge" json:"merge"`
}

// Repository is the per-member policy of a workspace Project.
type Repository struct {
	Review       string   `toml:"review" json:"review"`
	MergeMode    string   `toml:"merge_mode" json:"merge_mode"`
	TargetBranch string   `toml:"target_branch" json:"target_branch"`
	Gate         []string `toml:"gate" json:"gate"`
	Forge        string   `toml:"forge" json:"forge"`
}

type Lowkey struct {
	Lead bool `toml:"lead" json:"lead"`
}

type Config struct {
	Repositories        map[string]Repository `toml:"repositories" json:"repositories"`
	Lowkey              Lowkey                `toml:"lowkey" json:"lowkey"`
	Identity            Identity              `toml:"identity" json:"identity"`
	Lead                Lead                  `toml:"lead" json:"lead"`
	Defaults            Defaults              `toml:"defaults" json:"defaults"`
	ReviewLoop          ReviewLoop            `toml:"review_loop" json:"review_loop"`
	GateLoop            GateLoop              `toml:"gate_loop" json:"gate_loop"`
	CILoop              CILoop                `toml:"ci_loop" json:"ci_loop"`
	Pool                Pool                  `toml:"pool" json:"pool"`
	Retention           Retention             `toml:"retention" json:"retention"`
	Kinds               map[string]Kind       `toml:"kinds" json:"kinds"`
	Profiles            map[string]Profile    `toml:"profiles" json:"profiles"`
	Dispatch            []Rule                `toml:"dispatch_rules" json:"dispatch"`
	DispatchDefault     DispatchDefault       `toml:"dispatch_default" json:"dispatch_default"`
	Autonomy            Autonomy              `toml:"autonomy" json:"autonomy"`
	Files               []string              `toml:"-" json:"-"`
	LeadLanguageDefault bool                  `toml:"-" json:"-"`
}

type DispatchDefault struct {
	Use string `toml:"use" json:"use"`
}

type InvalidError struct {
	File   string
	Key    string
	Reason string
	Cause  error
}

func (e *InvalidError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.File, e.Key, e.Cause)
	}
	return fmt.Sprintf("%s: %s: %s", e.File, e.Key, e.Reason)
}

func (e *InvalidError) Unwrap() error { return e.Cause }

func DefaultHome() (string, error) {
	if home := os.Getenv("POSSE_HOME"); home != "" {
		return filepath.Clean(home), nil
	}
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(current.HomeDir, ".posse"), nil
}

func Load(home, projectName string) (Config, error) {
	return loadConfig(home, projectName, true)
}

// ValidateAll checks every stored User and Project config without writing
// vocabulary migrations or markers.
func ValidateAll(home string) error {
	if home == "" {
		var err error
		home, err = DefaultHome()
		if err != nil {
			return err
		}
	}
	if _, err := loadConfig(home, "", false); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(home, "projects"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		projectDir := filepath.Join(home, "projects", entry.Name())
		info, err := os.Stat(projectDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		path := filepath.Join(projectDir, "config.toml")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if _, err := loadConfig(home, entry.Name(), false); err != nil {
			return err
		}
	}
	return nil
}

// ValidationIssues reports every detectable issue in the global config and the
// selected Project config without writing vocabulary migrations or markers.
func ValidationIssues(home, projectName string) []error {
	if home == "" {
		var err error
		home, err = DefaultHome()
		if err != nil {
			return []error{err}
		}
	}
	globalPath := filepath.Join(home, "config.toml")
	global, globalErr := readConfigMap(globalPath, false)
	issues := []error{}
	if globalErr != nil {
		issues = append(issues, globalErr)
		global = map[string]any{}
	}
	if _, found := global["autonomy"]; found {
		issues = append(issues, &InvalidError{File: globalPath, Key: "autonomy", Reason: "autonomy is valid only in a Project config"})
	}
	issues = append(issues, SchemaValidationErrors(globalPath, global, false)...)

	projectPath := ""
	var project map[string]any
	var projectErr error
	if projectName != "" {
		projectPath = filepath.Join(home, "projects", projectName, "config.toml")
		project, projectErr = readConfigMap(projectPath, false)
		if projectErr != nil {
			issues = append(issues, projectErr)
			project = map[string]any{}
		}
		issues = append(issues, SchemaValidationErrors(projectPath, project, true)...)
	}
	if globalErr == nil && projectErr == nil {
		cfg, err := configFromMaps(globalPath, global, projectPath, project, projectName)
		if err != nil {
			issues = append(issues, err)
		} else {
			issues = append(issues, cfg.ValidationIssues(projectName != "")...)
		}
	}
	return uniqueValidationErrors(issues)
}

func uniqueValidationErrors(issues []error) []error {
	seen := make(map[string]struct{}, len(issues))
	unique := make([]error, 0, len(issues))
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		key := issue.Error()
		if _, found := seen[key]; found {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, issue)
	}
	return unique
}

func loadConfig(home, projectName string, migrate bool) (Config, error) {
	if home == "" {
		var err error
		home, err = DefaultHome()
		if err != nil {
			return Config{}, err
		}
	}
	globalPath := filepath.Join(home, "config.toml")
	global, err := readConfigMap(globalPath, migrate)
	if err != nil {
		return Config{}, err
	}
	if _, found := global["autonomy"]; found {
		return Config{}, &InvalidError{File: globalPath, Key: "autonomy", Reason: "autonomy is valid only in a Project config"}
	}
	if err := ValidateSchemaMap(globalPath, global, false); err != nil {
		return Config{}, err
	}
	projectPath := ""
	var project map[string]any
	if projectName != "" {
		projectPath = filepath.Join(home, "projects", projectName, "config.toml")
		project, err = readConfigMap(projectPath, migrate)
		if err != nil {
			return Config{}, err
		}
		if err := ValidateSchemaMap(projectPath, project, true); err != nil {
			return Config{}, err
		}
	}
	cfg, err := configFromMaps(globalPath, global, projectPath, project, projectName)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(projectName != ""); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func configFromMaps(globalPath string, global map[string]any, projectPath string, project map[string]any, projectName string) (Config, error) {
	globalRules, globalDefault := extractDispatch(global)
	merged := mergeMaps(defaultMap(), global)
	if projectName != "" {
		projectRules, projectDefault := extractDispatch(project)
		project = applyProjectScalars(project)
		merged = mergeMaps(merged, project)
		merged["dispatch_rules"] = append(projectRules, globalRules...)
		if projectDefault != "" {
			globalDefault = projectDefault
		}
	} else {
		merged["dispatch_rules"] = globalRules
	}
	if globalDefault != "" {
		merged["dispatch_default"] = map[string]any{"use": globalDefault}
	}
	var buffer bytes.Buffer
	if err := toml.NewEncoder(&buffer).Encode(merged); err != nil {
		return Config{}, &InvalidError{File: globalPath, Key: "config", Cause: err}
	}
	var cfg Config
	metadata, err := toml.Decode(buffer.String(), &cfg)
	if err != nil {
		file := globalPath
		if projectPath != "" {
			file = projectPath
		}
		return Config{}, &InvalidError{File: file, Key: tomlErrorKey(err), Cause: err}
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		field := undecoded[0]
		return Config{}, &InvalidError{File: globalPath, Key: field.String(), Reason: "unknown configuration key"}
	}
	cfg.LeadLanguageDefault = !hasLeadLanguage(global) && !hasLeadLanguage(project)
	cfg.Files = []string{globalPath}
	if projectPath != "" {
		cfg.Files = append(cfg.Files, projectPath)
	}
	cfg.normalizeBuiltins()
	if cfg.Autonomy.Yolo {
		cfg.Autonomy.Review = "lead"
		cfg.Autonomy.Merge = "auto"
	}
	return cfg, nil
}

func hasLeadLanguage(values map[string]any) bool {
	identity, _ := values["identity"].(map[string]any)
	lead, _ := identity["lead"].(map[string]any)
	_, found := lead["language"]
	return found
}

func readConfigMap(path string, migrate bool) (map[string]any, error) {
	if migrate {
		if err := migrateVocabularyFile(path); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if _, err := toml.Decode(string(data), &decoded); err != nil {
		return nil, &InvalidError{File: path, Key: tomlErrorKey(err), Cause: err}
	}
	if decoded == nil {
		decoded = map[string]any{}
	}
	if !migrate && !bytes.HasPrefix(data, []byte(vocabularyMarker)) {
		marker, markerErr := os.ReadFile(path + ".vocabulary")
		if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
			return nil, markerErr
		}
		if string(marker) != vocabularyMarker {
			if _, err := migrateVocabularyMap(decoded, ""); err != nil {
				return nil, &InvalidError{File: path, Key: "config", Cause: err}
			}
		}
	}
	return decoded, nil
}

func extractDispatch(values map[string]any) ([]any, string) {
	raw, found := values["dispatch"]
	if !found {
		return nil, ""
	}
	delete(values, "dispatch")
	entries, _ := raw.([]map[string]any)
	var rules []any
	defaultProfile := ""
	if table, ok := raw.(map[string]any); ok {
		if fallback, ok := table["default"].(map[string]any); ok {
			defaultProfile, _ = fallback["use"].(string)
		}
		return rules, defaultProfile
	}
	for _, entry := range entries {
		if fallback, ok := entry["default"].(map[string]any); ok {
			if use, ok := fallback["use"].(string); ok {
				defaultProfile = use
			}
			delete(entry, "default")
		}
		if len(entry) > 0 {
			rules = append(rules, entry)
		}
	}
	return rules, defaultProfile
}

func applyProjectScalars(project map[string]any) map[string]any {
	defaults, ok := project["defaults"].(map[string]any)
	if !ok {
		defaults = map[string]any{}
	}
	for _, key := range []string{"max_workers", "stall_after", "idle_after", "auto_teardown", "merge_mode", "merge_method", "forge", "pr_poll", "review", "gate"} {
		if value, found := project[key]; found {
			defaults[key] = value
			delete(project, key)
		}
	}
	if len(defaults) > 0 {
		project["defaults"] = defaults
	}
	return project
}

func mergeMaps(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		result[key] = clone(value)
	}
	for key, value := range override {
		if baseMap, ok := result[key].(map[string]any); ok {
			if overrideMap, overrideOK := value.(map[string]any); overrideOK {
				result[key] = mergeMaps(baseMap, overrideMap)
				continue
			}
		}
		result[key] = clone(value)
	}
	return result
}

func clone(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = clone(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(v))
		for i, item := range v {
			copyMap := clone(item).(map[string]any)
			out[i] = copyMap
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = clone(item)
		}
		return out
	default:
		return value
	}
}

func defaultMap() map[string]any {
	return map[string]any{
		"identity": map[string]any{
			"lead":   map[string]any{"name": "Lead", "persona": "", "language": "en", "address_user": ""},
			"worker": map[string]any{"display_prefix": "worker"},
		},
		"pool":        map[string]any{"clean": "warm", "setup": []string{}, "keep_idle": 4},
		"retention":   map[string]any{"task_artifacts": "2160h"},
		"review_loop": map[string]any{"enabled": true, "max_rounds": 3, "block_at": "P1", "auto_start": true, "docs": false, "guidance": []string{}},
		"gate_loop":   map[string]any{"max_fix_rounds": 3},
		"ci_loop":     map[string]any{"max_fix_rounds": 3, "rerun_cancelled": 1},
		"lead":        map[string]any{"kind": "", "profiles": map[string]any{}},
		"lowkey":      map[string]any{"lead": false},
		"defaults": map[string]any{
			"max_workers": 4, "dispatch_inheritance": "kind", "stall_after": "20m", "idle_after": "3m", "auto_teardown": "finished", "auto_recover": true, "recovery_attempts": 3, "recovery_backoff": "5s", "merge_mode": "pr",
			"merge_method": "squash", "forge": "auto", "pr_poll": "2m", "review": "none", "gate": []string{},
		},
		"kinds": map[string]any{
			"claude":   map[string]any{"disabled": false, "auto_approve_args": []string{"--dangerously-skip-permissions"}, "lead_auto_approve": true, "model_args": []string{"--model", "{model}"}, "effort_args": []string{"--effort", "{effort}"}, "resume_args": []string{"--resume", "{session}"}, "system_prompt_args": []string{"--append-system-prompt-file", "{file}"}, "prepare": "claude-trust", "notice_delivery": NoticeDeliveryWatch, "background_commands": true, "steer": true, "interrupt_keys": []string{"ctrl+c"}},
			"codex":    map[string]any{"disabled": false, "auto_approve_args": []string{"--dangerously-bypass-approvals-and-sandbox"}, "lead_auto_approve": true, "model_args": []string{"-m", "{model}"}, "effort_args": []string{"-c", "model_reasoning_effort={effort}"}, "resume_args": []string{"resume", "{session}"}, "system_prompt_args": []string{"-c", "developer_instructions={text}"}, "lead_args": []string{"--sandbox", "danger-full-access"}, "prepare": "codex-trust", "notice_delivery": NoticeDeliveryCodexQueue, "steer": true, "interrupt_keys": []string{"esc"}},
			"pi":       map[string]any{"disabled": false, "model_args": []string{"--model", "{model}"}, "effort_args": []string{"--thinking", "{effort}"}, "resume_args": []string{"--session", "{session}"}, "system_prompt_args": []string{"--append-system-prompt", "{file}"}, "notice_delivery": NoticeDeliveryPiExtension, "steer": true, "interrupt_keys": []string{"esc"}},
			"opencode": map[string]any{"disabled": false, "auto_approve_args": []string{"--auto"}, "lead_auto_approve": true, "model_args": []string{"--model", "{model}"}, "resume_args": []string{"--session", "{session}"}, "notice_delivery": NoticeDeliveryOpenCodePlugin, "interrupt_keys": []string{"esc"}},
		},
		"profiles":         map[string]any{},
		"dispatch_default": map[string]any{"use": ""},
	}
}

func (c *Config) normalizeBuiltins() {
	if c.Kinds == nil {
		c.Kinds = map[string]Kind{}
	}
	if _, ok := c.Kinds["claude"]; !ok {
		c.Kinds["claude"] = Kind{AutoApproveArgs: []string{"--dangerously-skip-permissions"}, LeadAutoApprove: true, ModelArgs: []string{"--model", "{model}"}, EffortArgs: []string{"--effort", "{effort}"}, ResumeArgs: []string{"--resume", "{session}"}, SystemPromptArgs: []string{"--append-system-prompt-file", "{file}"}, Prepare: "claude-trust", NoticeDelivery: NoticeDeliveryWatch, BackgroundCommands: true, Steer: true, InterruptKeys: []string{"ctrl+c"}}
	}
	if _, ok := c.Kinds["codex"]; !ok {
		c.Kinds["codex"] = Kind{AutoApproveArgs: []string{"--dangerously-bypass-approvals-and-sandbox"}, LeadAutoApprove: true, ModelArgs: []string{"-m", "{model}"}, EffortArgs: []string{"-c", "model_reasoning_effort={effort}"}, ResumeArgs: []string{"resume", "{session}"}, SystemPromptArgs: []string{"-c", "developer_instructions={text}"}, LeadArgs: []string{"--sandbox", "danger-full-access"}, Prepare: "codex-trust", NoticeDelivery: NoticeDeliveryCodexQueue, Steer: true, InterruptKeys: []string{"esc"}}
	}
	if _, ok := c.Kinds["pi"]; !ok {
		c.Kinds["pi"] = Kind{ModelArgs: []string{"--model", "{model}"}, EffortArgs: []string{"--thinking", "{effort}"}, ResumeArgs: []string{"--session", "{session}"}, SystemPromptArgs: []string{"--append-system-prompt", "{file}"}, NoticeDelivery: NoticeDeliveryPiExtension, Steer: true, InterruptKeys: []string{"esc"}}
	}
	if _, ok := c.Kinds["opencode"]; !ok {
		c.Kinds["opencode"] = Kind{AutoApproveArgs: []string{"--auto"}, LeadAutoApprove: true, ModelArgs: []string{"--model", "{model}"}, ResumeArgs: []string{"--session", "{session}"}, NoticeDelivery: NoticeDeliveryOpenCodePlugin, InterruptKeys: []string{"esc"}}
	}
	if c.Pool.Clean == "" {
		c.Pool.Clean = "warm"
	}
	if c.Retention.TaskArtifacts == "" {
		c.Retention.TaskArtifacts = "2160h"
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	if c.Repositories == nil {
		c.Repositories = map[string]Repository{}
	}
	if c.Lead.Profiles == nil {
		c.Lead.Profiles = map[string]string{}
	}
}

func (c Config) Validate(projectFile bool) error {
	issues := c.ValidationIssues(projectFile)
	if len(issues) == 0 {
		return nil
	}
	return issues[0]
}

// ValidationIssues returns all independent semantic config errors in stable
// order. Validate retains the first error for normal load and setup paths.
func (c Config) ValidationIssues(projectFile bool) []error {
	file := "config.toml"
	if len(c.Files) > 0 {
		file = c.Files[len(c.Files)-1]
	}
	var issues []error
	add := func(key, reason string) {
		issues = append(issues, &InvalidError{File: file, Key: key, Reason: reason})
	}
	if c.Defaults.MaxWorkers < 1 {
		add("defaults.max_workers", "must be positive")
	}
	if !oneOf(c.Defaults.DispatchInheritance, "kind", "lead_profile") {
		add("defaults.dispatch_inheritance", "must be kind or lead_profile")
	}
	if c.Defaults.RecoveryAttempts < 1 {
		add("defaults.recovery_attempts", "must be positive")
	}
	if backoff, err := time.ParseDuration(c.Defaults.RecoveryBackoff); err != nil || backoff <= 0 {
		add("defaults.recovery_backoff", "must be a positive duration")
	}
	if stall, err := time.ParseDuration(c.Defaults.StallAfter); err != nil || stall <= 0 {
		add("defaults.stall_after", "must be a positive duration")
	}
	if idle, err := time.ParseDuration(c.Defaults.IdleAfter); err != nil || idle <= 0 {
		add("defaults.idle_after", "must be a positive duration")
	}
	if prPoll, err := time.ParseDuration(c.Defaults.PRPoll); err != nil || prPoll <= 0 {
		add("defaults.pr_poll", "must be a positive duration")
	}
	if !oneOf(c.Defaults.AutoTeardown, "finished", "merged", "never") {
		add("defaults.auto_teardown", "must be one of finished, merged, never")
	}
	if !oneOf(c.Defaults.MergeMode, "local", "pr", "no-mistakes") {
		add("defaults.merge_mode", "must be local, pr or no-mistakes")
	}
	if !oneOf(c.Defaults.Forge, "", "auto", "github", "gitlab") {
		add("defaults.forge", "must be auto, github or gitlab")
	}
	for _, name := range sortedMapKeys(c.Repositories) {
		repo := c.Repositories[name]
		if repo.Review != "" {
			if err := ValidateReview(repo.Review); err != nil {
				add("repositories."+name+".review", err.Error())
			}
		}
		if !oneOf(repo.Forge, "", "auto", "github", "gitlab") {
			add("repositories."+name+".forge", "must be auto, github or gitlab")
		}
	}
	if !oneOf(c.Defaults.MergeMethod, "squash", "merge", "rebase") {
		add("defaults.merge_method", "must be squash, merge or rebase")
	}
	if err := ValidateReview(c.Defaults.Review); err != nil {
		add("defaults.review", err.Error())
	}
	if !oneOf(c.Pool.Clean, "warm", "pristine") {
		add("pool.clean", "must be warm or pristine")
	}
	if projectFile {
		if c.Autonomy.Yolo {
			c.Autonomy.Review = "lead"
			c.Autonomy.Merge = "auto"
		}
		if c.Autonomy.Review != "" && !oneOf(c.Autonomy.Review, "ask", "lead") {
			add("autonomy.review", "must be ask or lead")
		}
		if c.Autonomy.Merge != "" && !oneOf(c.Autonomy.Merge, "ask", "auto") {
			add("autonomy.merge", "must be ask or auto")
		}
	} else if !reflect.DeepEqual(c.Autonomy, Autonomy{}) {
		add("autonomy", "autonomy is valid only in a Project config")
	}
	if c.DispatchDefault.Use != "" {
		if _, ok := c.Profiles[c.DispatchDefault.Use]; !ok {
			add("dispatch.default.use", "references unknown Profile "+c.DispatchDefault.Use)
		}
	}
	for i, rule := range c.Dispatch {
		if rule.Type != "" && !oneOf(rule.Type, "change", "research", "review") {
			add(fmt.Sprintf("dispatch[%d].type", i), "must be change, research or review")
		}
		if rule.Use == "" {
			add(fmt.Sprintf("dispatch[%d].use", i), "must name a Profile")
		} else if _, ok := c.Profiles[rule.Use]; !ok {
			add(fmt.Sprintf("dispatch[%d].use", i), "references unknown Profile "+rule.Use)
		}
	}
	for _, name := range sortedMapKeys(c.Profiles) {
		profile := c.Profiles[name]
		if profile.Kind == "" {
			add("profiles."+name+".kind", "must name a Worker kind")
			continue
		}
		kind := c.Kinds[profile.Kind]
		profileFile := c.profileFile(name, file)
		if profile.Model != "" && len(kind.ModelArgs) == 0 {
			issues = append(issues, &InvalidError{File: profileFile, Key: "profiles." + name + ".model", Reason: "Worker kind has no model_args template"})
		}
		if profile.Effort != "" && len(kind.EffortArgs) == 0 {
			issues = append(issues, &InvalidError{File: profileFile, Key: "profiles." + name + ".effort", Reason: "Worker kind has no effort_args template"})
		}
	}
	for _, kind := range sortedMapKeys(c.Lead.Profiles) {
		profileName := c.Lead.Profiles[kind]
		if profileName == "" {
			add("lead.profiles."+kind, "must name a Profile")
			continue
		}
		profile, ok := c.Profiles[profileName]
		if !ok {
			add("lead.profiles."+kind, "references unknown Profile "+profileName)
			continue
		}
		if profile.Kind != kind {
			add("lead.profiles."+kind, "Profile kind must be "+kind)
		}
	}
	for _, name := range sortedMapKeys(c.Kinds) {
		kind := c.Kinds[name]
		if kind.NoticeDelivery != "" && !oneOf(kind.NoticeDelivery, NoticeDeliveries...) {
			add("kinds."+name+".notice_delivery", "must be one of "+strings.Join(NoticeDeliveries, ", "))
		}
		if name != "claude" && kind.NoticeDelivery == NoticeDeliveryWatch && !kind.BackgroundCommands {
			add("kinds."+name+".notice_delivery", "watch requires background_commands = true")
		}
	}
	if c.Pool.KeepIdle < 0 {
		add("pool.keep_idle", "cannot be negative")
	}
	return uniqueValidationErrors(issues)
}

func (c Config) profileFile(name, fallback string) string {
	for i := len(c.Files) - 1; i >= 0; i-- {
		contents, err := os.ReadFile(c.Files[i])
		if err != nil {
			continue
		}
		var values map[string]any
		if _, err := toml.Decode(string(contents), &values); err != nil {
			continue
		}
		if profiles, ok := values["profiles"].(map[string]any); ok {
			if _, found := profiles[name]; found {
				return c.Files[i]
			}
		}
	}
	return fallback
}

func oneOf(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if value == candidate {
			return true
		}
	}
	return false
}

func tomlErrorKey(err error) string {
	message := err.Error()
	if index := strings.Index(message, "(at "); index >= 0 {
		return strings.TrimSpace(message[:index])
	}
	return "config"
}

// EqualConfig is useful in tests that verify deep merge and map isolation.
func EqualConfig(left, right Config) bool { return reflect.DeepEqual(left, right) }
