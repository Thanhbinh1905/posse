package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type ValueType string

const (
	TypeString      ValueType = "string"
	TypeInteger     ValueType = "integer"
	TypeBoolean     ValueType = "boolean"
	TypeDuration    ValueType = "duration"
	TypeStringArray ValueType = "array of strings"
	TypeTableArray  ValueType = "array of tables"
)

type KeySpec struct {
	Key      string
	Type     ValueType
	Allowed  []string
	Default  any
	Meaning  string
	UserOnly bool
}

var keySpecs = []KeySpec{
	{Key: "identity.lead.name", Type: TypeString, Default: "Lead", Meaning: "Display name and Lead voice toward the User."},
	{Key: "identity.lead.persona", Type: TypeString, Default: "", Meaning: "Lead persona used only when speaking to the User."},
	{Key: "identity.lead.language", Type: TypeString, Default: "en", Meaning: "Language the Lead uses with the User."},
	{Key: "identity.lead.address_user", Type: TypeString, Default: "", Meaning: "How the Lead addresses the User."},
	{Key: "identity.worker.display_prefix", Type: TypeString, Default: "rider", Meaning: "Retained for compatibility; does not affect Herdr workspace labels."},
	{Key: "lead.kind", Type: TypeString, Default: "", Meaning: "Default agent kind used by posse up."},
	{Key: "lowkey.lead", Type: TypeBoolean, Default: false, Meaning: "Lowkey mode for the Lead: only report decisions and outcomes to the User. A display preference, not an autonomy grant."},
	{Key: "lead.profiles.<kind>", Type: TypeString, Meaning: "Profile used by the Lead for this agent kind."},
	{Key: "defaults.max_workers", Type: TypeInteger, Default: 4, Meaning: "Maximum concurrent Riders for a Project."},
	{Key: "defaults.stall_after", Type: TypeDuration, Default: "20m", Meaning: "Time without output or worktree progress before a Rider is stalled."},
	{Key: "defaults.idle_after", Type: TypeDuration, Default: "3m", Meaning: "Time a Rider may be idle without a Signal before a Notice is created."},
	{Key: "defaults.auto_unsaddle", Type: TypeString, Allowed: []string{"finished", "landed", "never"}, Default: "finished", Meaning: "Whether completed Tasks are automatically Teardown after landing or Notice acknowledgement."},
	{Key: "defaults.auto_recover", Type: TypeBoolean, Default: true, Meaning: "Whether plugin events and the Herdr startup hook restart the Lead and Riders after a Herdr restart or a closed Lead workspace; when false, `posse up` recovers them."},
	{Key: "defaults.recovery_attempts", Type: TypeInteger, Default: 3, Meaning: "Maximum automatic relaunch attempts per Rider recovery episode; exhaustion requires explicit relaunch."},
	{Key: "defaults.recovery_backoff", Type: TypeDuration, Default: "5s", Meaning: "Initial delay between failed automatic Rider relaunches; doubles up to one minute."},
	{Key: "defaults.landing_mode", Type: TypeString, Allowed: []string{"local", "pr", "no-mistakes"}, Default: "pr", Meaning: "How Ship Task changes are Landed."},
	{Key: "defaults.forge", Type: TypeString, Allowed: []string{"auto", "github", "gitlab"}, Default: "auto", Meaning: "Forge for this Project; auto detects from each repository remote."},
	{Key: "repositories.<repository>.forge", Type: TypeString, Allowed: []string{"auto", "github", "gitlab"}, Default: "auto", Meaning: "Override the forge for a member repository."},
	{Key: "defaults.merge_method", Type: TypeString, Allowed: []string{"squash", "merge", "rebase"}, Default: "squash", Meaning: "Merge method used for pull requests."},
	{Key: "defaults.pr_poll", Type: TypeDuration, Default: "2m", Meaning: "Minimum interval between pull request and Project checkout polls."},
	{Key: "defaults.review", Type: TypeString, Allowed: []string{"always", "on_risk", "never"}, Default: "on_risk", Meaning: "When a completed Ship Task requires review."},
	{Key: "defaults.gate", Type: TypeStringArray, Default: []string{}, Meaning: "Commands run in the Mount before a Ship Task may Land.", UserOnly: true},
	{Key: "repositories.<repo>.landing_mode", Type: TypeString, Allowed: []string{"local", "pr"}, Meaning: "How this member repository of a workspace Project Lands; defaults to local without an origin, else the Project landing_mode."},
	{Key: "repositories.<repo>.gate", Type: TypeStringArray, Default: []string{}, Meaning: "Commands run in this member's worktree before it may Land; replaces defaults.gate for the member.", UserOnly: true},
	{Key: "remuda.clean", Type: TypeString, Allowed: []string{"warm", "pristine"}, Default: "warm", Meaning: "Whether released Mounts keep ignored files."},
	{Key: "remuda.setup", Type: TypeStringArray, Default: []string{}, Meaning: "Commands run in a Mount after it is acquired.", UserOnly: true},
	{Key: "remuda.keep_idle", Type: TypeInteger, Default: 4, Meaning: "Idle Mounts kept for each Project."},
	{Key: "kinds.<kind>.auto_approve_args", Type: TypeStringArray, Default: []string{}, Meaning: "Arguments that enable automatic approvals for this agent kind; Leads also get them by default when lead_auto_approve is true."},
	{Key: "kinds.<kind>.lead_auto_approve", Type: TypeBoolean, Default: map[string]bool{"claude": true, "codex": true, "opencode": true}, Meaning: "Whether Leads of this kind also get auto_approve_args; set false to keep the harness's approval prompts. User-only. Pi has no permission system.", UserOnly: true},
	{Key: "kinds.<kind>.model_args", Type: TypeStringArray, Default: []string{}, Meaning: "Argument template for a Profile model; supports {model}."},
	{Key: "kinds.<kind>.effort_args", Type: TypeStringArray, Default: []string{}, Meaning: "Argument template for a Profile effort; supports {effort}."},
	{Key: "kinds.<kind>.resume_args", Type: TypeStringArray, Default: []string{}, Meaning: "Argument template for resuming a recorded agent session; supports {session}."},
	{Key: "kinds.<kind>.system_prompt_args", Type: TypeStringArray, Default: map[string][]string{"claude": {"--append-system-prompt-file", "{file}"}, "codex": {"-c", "developer_instructions={text}"}, "pi": {"--append-system-prompt", "{file}"}}, Meaning: "Argument template that gives the Lead its instructions as a system prompt; supports {file} (the instructions file) and {text} (the complete instructions with whitespace preserved; Codex receives a TOML-quoted string)."},
	{Key: "kinds.<kind>.lead_args", Type: TypeStringArray, Default: map[string][]string{"codex": {"--sandbox", "danger-full-access"}}, Meaning: "Arguments every Lead of this kind gets so it can run posse commands; codex's default sandbox cannot write the posse home. Approvals are unchanged."},
	{Key: "kinds.<kind>.background_commands", Type: TypeBoolean, Default: map[string]bool{"claude": true, "codex": false, "pi": false}, Meaning: "Whether this agent kind can run a background command and be re-invoked when it exits; Riders of such kinds are told to background long commands."},
	{Key: "kinds.<kind>.notice_delivery", Type: TypeString, Allowed: NoticeDeliveries, Default: map[string]string{"claude": NoticeDeliveryLookout, "codex": NoticeDeliveryCodexQueue, "pi": NoticeDeliveryPiExtension, "opencode": NoticeDeliveryOpenCodePlugin, "<other>": NoticeDeliveryPrompt}, Meaning: "How Notices reach a Lead: prompt (typed only when idle and unfocused), lookout (background command), codex-queue (codex session queue), pi-extension (Pi lookout extension), opencode-plugin (Lead-only OpenCode lookout and session API)."},
	{Key: "kinds.<kind>.steer", Type: TypeBoolean, Default: map[string]bool{"claude": true, "codex": true, "pi": true}, Meaning: "Allow sending to an unfocused Rider while it is working; unknown kinds default to false."},
	{Key: "kinds.<kind>.prepare", Type: TypeString, Allowed: []string{"", "claude-trust", "codex-trust"}, Default: "", Meaning: "Built-in preparation step run before starting an agent."},
	{Key: "profiles.<profile>.kind", Type: TypeString, Meaning: "Agent kind used by this Profile."},
	{Key: "profiles.<profile>.model", Type: TypeString, Default: "", Meaning: "Model argument value supplied through the kind template."},
	{Key: "profiles.<profile>.effort", Type: TypeString, Default: "", Meaning: "Reasoning effort supplied through the kind template."},
	{Key: "profiles.<profile>.args", Type: TypeStringArray, Default: []string{}, Meaning: "Additional arguments passed to this Profile's agent."},
	{Key: "dispatch", Type: TypeTableArray, Default: []any{}, Meaning: "Ordered dispatch rules that select a Profile."},
	{Key: "dispatch.default.use", Type: TypeString, Default: "", Meaning: "Fallback Profile used when no dispatch rule matches."},
	{Key: "autonomy.yolo", Type: TypeBoolean, Default: false, Meaning: "Shorthand granting the Lead review and landing autonomy.", UserOnly: true},
	{Key: "autonomy.review", Type: TypeString, Allowed: []string{"ask", "lead"}, Default: "ask", Meaning: "Whether the Lead may decide review findings without asking the User.", UserOnly: true},
	{Key: "autonomy.land", Type: TypeString, Allowed: []string{"ask", "auto"}, Default: "ask", Meaning: "Whether the Lead may Land changes without asking the User.", UserOnly: true},
}

var namePart = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func Schema() []KeySpec {
	result := make([]KeySpec, len(keySpecs))
	copy(result, keySpecs)
	for i := range result {
		result[i].Allowed = append([]string(nil), result[i].Allowed...)
	}
	return result
}

func Lookup(key string) (KeySpec, bool) {
	for _, spec := range keySpecs {
		if schemaKeyMatches(spec.Key, key) {
			return spec, true
		}
	}
	return KeySpec{}, false
}

func IsUserOnly(key string) bool {
	spec, found := Lookup(key)
	return found && spec.UserOnly
}

func ValidateSetting(key string, value any, projectFile bool) error {
	spec, found := Lookup(key)
	if !found {
		return fmt.Errorf("unknown configuration key %q", key)
	}
	if strings.HasPrefix(key, "autonomy.") && !projectFile {
		return fmt.Errorf("autonomy is valid only in a Project config")
	}
	valid := false
	switch spec.Type {
	case TypeString, TypeDuration:
		_, valid = value.(string)
	case TypeInteger:
		switch value.(type) {
		case int64, int, int32:
			valid = true
		}
	case TypeBoolean:
		_, valid = value.(bool)
	case TypeStringArray:
		valid = stringArray(value)
	case TypeTableArray:
		valid = dispatchRules(value)
	}
	if !valid {
		return fmt.Errorf("%s must be %s", key, spec.Type)
	}
	if text, ok := value.(string); ok {
		if len(spec.Allowed) > 0 && !oneOf(text, spec.Allowed...) {
			return fmt.Errorf("%s must be one of %s", key, strings.Join(spec.Allowed, ", "))
		}
		if spec.Type == TypeDuration {
			parsed, err := time.ParseDuration(text)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("%s must be a positive duration", key)
			}
		}
	}
	if key == "defaults.max_workers" || key == "remuda.keep_idle" || key == "defaults.recovery_attempts" {
		if number, ok := integerValue(value); ok && number < 1 {
			return fmt.Errorf("%s must be positive", key)
		}
	}
	if strings.HasPrefix(key, "profiles.") && strings.HasSuffix(key, ".kind") {
		if text, ok := value.(string); ok && text == "" {
			return fmt.Errorf("%s must name an agent kind", key)
		}
	}
	return nil
}

func ValidateSchemaMap(file string, values map[string]any, projectFile bool) error {
	var walk func(string, any) error
	walk = func(prefix string, value any) error {
		switch current := value.(type) {
		case map[string]any:
			if prefix == "dispatch" {
				for key, item := range current {
					if key != "default" {
						return &InvalidError{File: file, Key: "dispatch." + key, Reason: "unknown configuration key"}
					}
					if err := walk("dispatch.default", item); err != nil {
						return err
					}
				}
				return nil
			}
			if !knownContainer(prefix) && prefix != "" {
				return &InvalidError{File: file, Key: prefix, Reason: "unknown configuration table"}
			}
			for key, item := range current {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				if err := walk(path, item); err != nil {
					return err
				}
			}
		case []map[string]any:
			if prefix != "dispatch" {
				return fmt.Errorf("%s: %s must be an array of tables", file, prefix)
			}
			for index, entry := range current {
				for key, item := range entry {
					path := "dispatch." + key
					if path == "dispatch.default" {
						continue
					}
					if err := validateDispatchValue(file, path, item, index); err != nil {
						return err
					}
				}
			}
		default:
			if prefix == "" {
				return nil
			}
			key := canonicalSchemaKey(prefix)
			if _, found := Lookup(key); !found {
				return &InvalidError{File: file, Key: prefix, Reason: "unknown configuration key"}
			}
			if err := ValidateSetting(key, value, projectFile); err != nil {
				return &InvalidError{File: file, Key: key, Reason: strings.TrimPrefix(err.Error(), key+" ")}
			}
		}
		return nil
	}
	if values == nil {
		return nil
	}
	for key, value := range values {
		if !projectFile && isProjectScalarAlias(key) {
			return &InvalidError{File: file, Key: key, Reason: "is valid only in a Project config"}
		}
		path := key
		if err := walk(path, value); err != nil {
			var invalid *InvalidError
			if errors.As(err, &invalid) {
				return err
			}
			return &InvalidError{File: file, Key: path, Reason: strings.TrimPrefix(err.Error(), file+": ")}
		}
	}
	return nil
}

func validateDispatchValue(file, key string, value any, index int) error {
	valid := false
	switch key {
	case "dispatch.type":
		text, ok := value.(string)
		valid = ok && oneOf(text, "ship", "scout", "review")
	case "dispatch.when", "dispatch.use":
		_, valid = value.(string)
	}
	if !valid {
		return &InvalidError{File: file, Key: fmt.Sprintf("dispatch[%d].%s", index, strings.TrimPrefix(key, "dispatch.")), Reason: "has an invalid type or value"}
	}
	return nil
}

func schemaKeyMatches(pattern, key string) bool {
	patternParts, keyParts := strings.Split(pattern, "."), strings.Split(key, ".")
	if len(patternParts) != len(keyParts) {
		return false
	}
	for i, part := range patternParts {
		if strings.HasPrefix(part, "<") && strings.HasSuffix(part, ">") {
			if !namePart.MatchString(keyParts[i]) {
				return false
			}
			continue
		}
		if part != keyParts[i] {
			return false
		}
	}
	return true
}

func canonicalSchemaKey(key string) string {
	switch key {
	case "max_workers", "stall_after", "idle_after", "auto_unsaddle", "landing_mode", "merge_method", "forge", "pr_poll", "review", "gate":
		return "defaults." + key
	}
	return key
}

func knownContainer(prefix string) bool {
	for _, spec := range keySpecs {
		parts := strings.Split(spec.Key, ".")
		if len(parts) <= 1 {
			continue
		}
		for i := 1; i < len(parts); i++ {
			if schemaKeyMatches(strings.Join(parts[:i], "."), prefix) {
				return true
			}
		}
	}
	return prefix == "dispatch.default"
}

func isProjectScalarAlias(key string) bool {
	switch key {
	case "landing_mode", "max_workers", "stall_after", "idle_after", "auto_unsaddle", "merge_method", "forge", "pr_poll", "review", "gate":
		return true
	default:
		return false
	}
}

func stringArray(value any) bool {
	switch list := value.(type) {
	case []string:
		return true
	case []any:
		for _, item := range list {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// dispatchRules accepts only rules made of the string fields type, when and
// use, so nothing a rule states is dropped when the rule is written.
func dispatchRules(value any) bool {
	entries, ok := tableEntries(value)
	if !ok {
		return false
	}
	for _, entry := range entries {
		for key, field := range entry {
			if _, isString := field.(string); !isString || key != "type" && key != "when" && key != "use" {
				return false
			}
		}
	}
	return true
}

func integerValue(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	default:
		return 0, false
	}
}
