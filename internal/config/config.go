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
	MaxWorkers   int      `toml:"max_workers" json:"max_workers"`
	StallAfter   string   `toml:"stall_after" json:"stall_after"`
	IdleAfter    string   `toml:"idle_after" json:"idle_after"`
	AutoUnsaddle string   `toml:"auto_unsaddle" json:"auto_unsaddle"`
	AutoRecover  bool     `toml:"auto_recover" json:"auto_recover"`
	LandingMode  string   `toml:"landing_mode" json:"landing_mode"`
	MergeMethod  string   `toml:"merge_method" json:"merge_method"`
	Forge        string   `toml:"forge" json:"forge"`
	PRPoll       string   `toml:"pr_poll" json:"pr_poll"`
	Review       string   `toml:"review" json:"review"`
	Gate         []string `toml:"gate" json:"gate"`
}

type Remuda struct {
	Clean    string   `toml:"clean" json:"clean"`
	Setup    []string `toml:"setup" json:"setup"`
	KeepIdle int      `toml:"keep_idle" json:"keep_idle"`
}

type Kind struct {
	AutoApproveArgs    []string `toml:"auto_approve_args" json:"auto_approve_args"`
	ModelArgs          []string `toml:"model_args" json:"model_args"`
	EffortArgs         []string `toml:"effort_args" json:"effort_args"`
	ResumeArgs         []string `toml:"resume_args" json:"resume_args"`
	SystemPromptArgs   []string `toml:"system_prompt_args" json:"system_prompt_args"`
	LeadArgs           []string `toml:"lead_args" json:"lead_args"`
	Prepare            string   `toml:"prepare" json:"prepare"`
	NoticeDelivery     string   `toml:"notice_delivery" json:"notice_delivery"`
	BackgroundCommands bool     `toml:"background_commands" json:"background_commands"`
	Steer              bool     `toml:"steer" json:"steer"`
}

// Notice delivery modes: how Notices reach a Lead of this kind. An empty value
// means NoticeDeliveryPrompt.
const (
	NoticeDeliveryPrompt         = "prompt"          // type a prompt when the pane is idle and unfocused
	NoticeDeliveryLookout        = "lookout"         // the Lead keeps a background `posse lookout`
	NoticeDeliveryCodexQueue     = "codex-queue"     // posse queues the prompt with `codex queue`
	NoticeDeliveryPiExtension    = "pi-extension"    // a posse pi extension runs `posse lookout`
	NoticeDeliveryOpenCodePlugin = "opencode-plugin" // a Lead-only plugin queues lookout batches through the session API
)

var NoticeDeliveries = []string{NoticeDeliveryPrompt, NoticeDeliveryLookout, NoticeDeliveryCodexQueue, NoticeDeliveryPiExtension, NoticeDeliveryOpenCodePlugin}

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
	Land   string `toml:"land" json:"land"`
}

// Repository is the per-member policy of a workspace Project.
type Repository struct {
	LandingMode string   `toml:"landing_mode" json:"landing_mode"`
	Gate        []string `toml:"gate" json:"gate"`
	Forge       string   `toml:"forge" json:"forge"`
}

type Lowkey struct {
	Lead bool `toml:"lead" json:"lead"`
}

type Config struct {
	Repositories    map[string]Repository `toml:"repositories" json:"repositories"`
	Lowkey          Lowkey                `toml:"lowkey" json:"lowkey"`
	Identity        Identity              `toml:"identity" json:"identity"`
	Lead            Lead                  `toml:"lead" json:"lead"`
	Defaults        Defaults              `toml:"defaults" json:"defaults"`
	Remuda          Remuda                `toml:"remuda" json:"remuda"`
	Kinds           map[string]Kind       `toml:"kinds" json:"kinds"`
	Profiles        map[string]Profile    `toml:"profiles" json:"profiles"`
	Dispatch        []Rule                `toml:"dispatch_rules" json:"dispatch"`
	DispatchDefault DispatchDefault       `toml:"dispatch_default" json:"dispatch_default"`
	Autonomy        Autonomy              `toml:"autonomy" json:"autonomy"`
	Files           []string              `toml:"-" json:"-"`
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
	if home == "" {
		var err error
		home, err = DefaultHome()
		if err != nil {
			return Config{}, err
		}
	}
	globalPath := filepath.Join(home, "config.toml")
	global, err := readMap(globalPath)
	if err != nil {
		return Config{}, err
	}
	if _, found := global["autonomy"]; found {
		return Config{}, &InvalidError{File: globalPath, Key: "autonomy", Reason: "autonomy is valid only in a Project config"}
	}
	if err := ValidateSchemaMap(globalPath, global, false); err != nil {
		return Config{}, err
	}
	globalRules, globalDefault := extractDispatch(global)
	merged := mergeMaps(defaultMap(), global)
	projectPath := ""
	var project map[string]any
	if projectName != "" {
		projectPath = filepath.Join(home, "projects", projectName, "config.toml")
		project, err = readMap(projectPath)
		if err != nil {
			return Config{}, err
		}
		if err := ValidateSchemaMap(projectPath, project, true); err != nil {
			return Config{}, err
		}
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
	cfg.Files = []string{globalPath}
	if projectPath != "" {
		cfg.Files = append(cfg.Files, projectPath)
	}
	cfg.normalizeBuiltins()
	if cfg.Autonomy.Yolo {
		cfg.Autonomy.Review = "lead"
		cfg.Autonomy.Land = "auto"
	}
	if err := cfg.Validate(projectName != ""); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readMap(path string) (map[string]any, error) {
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
	for _, key := range []string{"max_workers", "stall_after", "idle_after", "auto_unsaddle", "landing_mode", "merge_method", "forge", "pr_poll", "review", "gate"} {
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
			"worker": map[string]any{"display_prefix": "rider"},
		},
		"remuda": map[string]any{"clean": "warm", "setup": []string{}, "keep_idle": 4},
		"lead":   map[string]any{"kind": "", "profiles": map[string]any{}},
		"lowkey": map[string]any{"lead": false},
		"defaults": map[string]any{
			"max_workers": 4, "stall_after": "20m", "idle_after": "3m", "auto_unsaddle": "finished", "auto_recover": true, "landing_mode": "pr",
			"merge_method": "squash", "forge": "auto", "pr_poll": "2m", "review": "on_risk", "gate": []string{},
		},
		"kinds": map[string]any{
			"claude":   map[string]any{"auto_approve_args": []string{"--dangerously-skip-permissions"}, "model_args": []string{"--model", "{model}"}, "effort_args": []string{"--effort", "{effort}"}, "resume_args": []string{"--resume", "{session}"}, "system_prompt_args": []string{"--append-system-prompt-file", "{file}"}, "prepare": "claude-trust", "notice_delivery": NoticeDeliveryLookout, "background_commands": true, "steer": true},
			"codex":    map[string]any{"auto_approve_args": []string{"--dangerously-bypass-approvals-and-sandbox"}, "model_args": []string{"-m", "{model}"}, "effort_args": []string{"-c", "model_reasoning_effort={effort}"}, "resume_args": []string{"resume", "{session}"}, "system_prompt_args": []string{"-c", "developer_instructions={text}"}, "lead_args": []string{"--sandbox", "danger-full-access"}, "prepare": "codex-trust", "notice_delivery": NoticeDeliveryCodexQueue, "steer": true},
			"pi":       map[string]any{"model_args": []string{"--model", "{model}"}, "effort_args": []string{"--thinking", "{effort}"}, "resume_args": []string{"--session", "{session}"}, "system_prompt_args": []string{"--append-system-prompt", "{file}"}, "notice_delivery": NoticeDeliveryPiExtension, "steer": true},
			"opencode": map[string]any{"auto_approve_args": []string{"--auto"}, "model_args": []string{"--model", "{model}"}, "resume_args": []string{"--session", "{session}"}, "notice_delivery": NoticeDeliveryOpenCodePlugin},
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
		c.Kinds["claude"] = Kind{AutoApproveArgs: []string{"--dangerously-skip-permissions"}, ModelArgs: []string{"--model", "{model}"}, EffortArgs: []string{"--effort", "{effort}"}, ResumeArgs: []string{"--resume", "{session}"}, SystemPromptArgs: []string{"--append-system-prompt-file", "{file}"}, Prepare: "claude-trust", NoticeDelivery: NoticeDeliveryLookout, BackgroundCommands: true, Steer: true}
	}
	if _, ok := c.Kinds["codex"]; !ok {
		c.Kinds["codex"] = Kind{AutoApproveArgs: []string{"--dangerously-bypass-approvals-and-sandbox"}, ModelArgs: []string{"-m", "{model}"}, EffortArgs: []string{"-c", "model_reasoning_effort={effort}"}, ResumeArgs: []string{"resume", "{session}"}, SystemPromptArgs: []string{"-c", "developer_instructions={text}"}, LeadArgs: []string{"--sandbox", "danger-full-access"}, Prepare: "codex-trust", NoticeDelivery: NoticeDeliveryCodexQueue, Steer: true}
	}
	if _, ok := c.Kinds["pi"]; !ok {
		c.Kinds["pi"] = Kind{ModelArgs: []string{"--model", "{model}"}, EffortArgs: []string{"--thinking", "{effort}"}, ResumeArgs: []string{"--session", "{session}"}, SystemPromptArgs: []string{"--append-system-prompt", "{file}"}, NoticeDelivery: NoticeDeliveryPiExtension, Steer: true}
	}
	if _, ok := c.Kinds["opencode"]; !ok {
		c.Kinds["opencode"] = Kind{AutoApproveArgs: []string{"--auto"}, ModelArgs: []string{"--model", "{model}"}, ResumeArgs: []string{"--session", "{session}"}, NoticeDelivery: NoticeDeliveryOpenCodePlugin}
	}
	if c.Remuda.Clean == "" {
		c.Remuda.Clean = "warm"
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
	file := "config.toml"
	if len(c.Files) > 0 {
		file = c.Files[len(c.Files)-1]
	}
	if c.Defaults.MaxWorkers < 1 {
		return &InvalidError{File: file, Key: "defaults.max_workers", Reason: "must be positive"}
	}
	stall, err := time.ParseDuration(c.Defaults.StallAfter)
	if err != nil || stall <= 0 {
		return &InvalidError{File: file, Key: "defaults.stall_after", Reason: "must be a positive duration"}
	}
	idle, err := time.ParseDuration(c.Defaults.IdleAfter)
	if err != nil || idle <= 0 {
		return &InvalidError{File: file, Key: "defaults.idle_after", Reason: "must be a positive duration"}
	}
	prPoll, err := time.ParseDuration(c.Defaults.PRPoll)
	if err != nil || prPoll <= 0 {
		return &InvalidError{File: file, Key: "defaults.pr_poll", Reason: "must be a positive duration"}
	}
	if !oneOf(c.Defaults.AutoUnsaddle, "finished", "landed", "never") {
		return &InvalidError{File: file, Key: "defaults.auto_unsaddle", Reason: "must be one of finished, landed, never"}
	}
	if !oneOf(c.Defaults.LandingMode, "local", "pr", "no-mistakes") {
		return &InvalidError{File: file, Key: "defaults.landing_mode", Reason: "must be local, pr or no-mistakes"}
	}
	if !oneOf(c.Defaults.Forge, "", "auto", "github", "gitlab") {
		return &InvalidError{File: file, Key: "defaults.forge", Reason: "must be auto, github or gitlab"}
	}
	for name, repo := range c.Repositories {
		if !oneOf(repo.Forge, "", "auto", "github", "gitlab") {
			return &InvalidError{File: file, Key: "repositories." + name + ".forge", Reason: "must be auto, github or gitlab"}
		}
	}
	if !oneOf(c.Defaults.MergeMethod, "squash", "merge", "rebase") {
		return &InvalidError{File: file, Key: "defaults.merge_method", Reason: "must be squash, merge or rebase"}
	}
	if !oneOf(c.Defaults.Review, "always", "on_risk", "never") {
		return &InvalidError{File: file, Key: "defaults.review", Reason: "must be always, on_risk or never"}
	}
	if !oneOf(c.Remuda.Clean, "warm", "pristine") {
		return &InvalidError{File: file, Key: "remuda.clean", Reason: "must be warm or pristine"}
	}
	if projectFile {
		if c.Autonomy.Yolo {
			c.Autonomy.Review = "lead"
			c.Autonomy.Land = "auto"
		}
		if c.Autonomy.Review != "" && !oneOf(c.Autonomy.Review, "ask", "lead") {
			return &InvalidError{File: file, Key: "autonomy.review", Reason: "must be ask or lead"}
		}
		if c.Autonomy.Land != "" && !oneOf(c.Autonomy.Land, "ask", "auto") {
			return &InvalidError{File: file, Key: "autonomy.land", Reason: "must be ask or auto"}
		}
	} else if !reflect.DeepEqual(c.Autonomy, Autonomy{}) {
		return &InvalidError{File: file, Key: "autonomy", Reason: "autonomy is valid only in a Project config"}
	}
	if c.DispatchDefault.Use != "" {
		if _, ok := c.Profiles[c.DispatchDefault.Use]; !ok {
			return &InvalidError{File: file, Key: "dispatch.default.use", Reason: "references unknown Profile " + c.DispatchDefault.Use}
		}
	}
	for i, rule := range c.Dispatch {
		if rule.Type != "" && !oneOf(rule.Type, "ship", "scout", "review") {
			return &InvalidError{File: file, Key: fmt.Sprintf("dispatch[%d].type", i), Reason: "must be ship, scout or review"}
		}
		if rule.Use == "" {
			return &InvalidError{File: file, Key: fmt.Sprintf("dispatch[%d].use", i), Reason: "must name a Profile"}
		}
		if _, ok := c.Profiles[rule.Use]; !ok {
			return &InvalidError{File: file, Key: fmt.Sprintf("dispatch[%d].use", i), Reason: "references unknown Profile " + rule.Use}
		}
	}
	for name, profile := range c.Profiles {
		if profile.Kind == "" {
			return &InvalidError{File: file, Key: "profiles." + name + ".kind", Reason: "must name a Rider kind"}
		}
		kind := c.Kinds[profile.Kind]
		profileFile := c.profileFile(name, file)
		if profile.Model != "" && len(kind.ModelArgs) == 0 {
			return &InvalidError{File: profileFile, Key: "profiles." + name + ".model", Reason: "Rider kind has no model_args template"}
		}
		if profile.Effort != "" && len(kind.EffortArgs) == 0 {
			return &InvalidError{File: profileFile, Key: "profiles." + name + ".effort", Reason: "Rider kind has no effort_args template"}
		}
	}
	for kind, profileName := range c.Lead.Profiles {
		if profileName == "" {
			return &InvalidError{File: file, Key: "lead.profiles." + kind, Reason: "must name a Profile"}
		}
		profile, ok := c.Profiles[profileName]
		if !ok {
			return &InvalidError{File: file, Key: "lead.profiles." + kind, Reason: "references unknown Profile " + profileName}
		}
		if profile.Kind != kind {
			return &InvalidError{File: file, Key: "lead.profiles." + kind, Reason: "Profile kind must be " + kind}
		}
	}
	for name, kind := range c.Kinds {
		if kind.NoticeDelivery != "" && !oneOf(kind.NoticeDelivery, NoticeDeliveries...) {
			return &InvalidError{File: file, Key: "kinds." + name + ".notice_delivery", Reason: "must be one of " + strings.Join(NoticeDeliveries, ", ")}
		}
		if kind.NoticeDelivery == NoticeDeliveryLookout && !kind.BackgroundCommands {
			return &InvalidError{File: file, Key: "kinds." + name + ".notice_delivery", Reason: "lookout requires background_commands = true"}
		}
	}
	if !oneOf(c.Remuda.Clean, "warm", "pristine") || c.Remuda.KeepIdle < 0 {
		return &InvalidError{File: file, Key: "remuda", Reason: "clean must be warm or pristine and keep_idle cannot be negative"}
	}
	return nil
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
