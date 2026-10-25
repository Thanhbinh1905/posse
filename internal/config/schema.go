package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
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
	{Key: "identity.lead.language", Type: TypeString, Default: "en", Meaning: "Language for Lead messages to the User and neutral prose in Briefs, send instructions, reviews, decisions, Reports, commit messages, Signal notes and summaries, and other evidence."},
	{Key: "identity.lead.address_user", Type: TypeString, Default: "", Meaning: "How the Lead addresses the User."},
	{Key: "identity.worker.display_prefix", Type: TypeString, Default: "worker", Meaning: "Deprecated; retained for compatibility and has no effect on Herdr labels or Task names."},
	{Key: "lead.kind", Type: TypeString, Default: "", Meaning: "Default agent kind used by posse up."},
	{Key: "lowkey.lead", Type: TypeBoolean, Default: false, Meaning: "Lowkey mode for the Lead: only report decisions and outcomes to the User. A display preference, not an autonomy grant."},
	{Key: "knowledge.enabled", Type: TypeBoolean, Default: false, Meaning: "Load curated User and Project knowledge into Lead instructions and new Worker launches, and allow knowledge writes. Knowledge is reference context, not authority (ADR 0032).", UserOnly: true},
	{Key: "knowledge.budget_bytes", Type: TypeInteger, Default: 8192, Meaning: "Startup budget for always-loaded knowledge plus the same role's preferences; a knowledge write that grows an over-budget total is refused until entries are curated."},
	{Key: "lead.profiles.<kind>", Type: TypeString, Meaning: "Profile used by the Lead for this agent kind."},
	{Key: "defaults.max_workers", Type: TypeInteger, Default: 4, Meaning: "Maximum concurrent Workers for a Project."},
	{Key: "defaults.dispatch_inheritance", Type: TypeString, Allowed: []string{"kind", "lead_profile"}, Default: "kind", Meaning: "Profile inherited when no Dispatch Rule matches: the Lead's agent kind only, or its resolved Profile."},
	{Key: "defaults.stall_after", Type: TypeDuration, Default: "20m", Meaning: "Time without output or worktree progress before a Worker is stalled."},
	{Key: "defaults.idle_after", Type: TypeDuration, Default: "3m", Meaning: "Time a Worker may be idle without a Signal before a Notice is created."},
	{Key: "defaults.auto_teardown", Type: TypeString, Allowed: []string{"finished", "merged", "never"}, Default: "finished", Meaning: "Whether completed Tasks are automatically Teardown after merging or Notice acknowledgement."},
	{Key: "defaults.auto_recover", Type: TypeBoolean, Default: true, Meaning: "Whether plugin events and the Herdr startup hook restart the Lead and Workers after a Herdr restart or a closed Lead workspace; when false, `posse up` recovers them."},
	{Key: "defaults.recovery_attempts", Type: TypeInteger, Default: 3, Meaning: "Maximum automatic relaunch attempts per Worker recovery episode; exhaustion requires explicit relaunch."},
	{Key: "defaults.recovery_backoff", Type: TypeDuration, Default: "5s", Meaning: "Initial delay between failed automatic Worker relaunches; doubles up to one minute."},
	{Key: "defaults.merge_mode", Type: TypeString, Allowed: []string{"local", "pr"}, Default: "pr", Meaning: "How Change Task changes are Merged."},
	{Key: "defaults.target_branch", Type: TypeString, Default: "", Meaning: "Default PR/MR target branch for Change Tasks; empty uses each repository's default branch."},
	{Key: "defaults.forge", Type: TypeString, Allowed: []string{"auto", "github", "gitlab"}, Default: "auto", Meaning: "Forge for this Project; auto detects from each repository remote."},
	{Key: "repositories.<repository>.forge", Type: TypeString, Allowed: []string{"auto", "github", "gitlab"}, Default: "auto", Meaning: "Override the forge for a member repository."},
	{Key: "defaults.merge_method", Type: TypeString, Allowed: []string{"squash", "merge", "rebase"}, Default: "squash", Meaning: "Merge method used for pull requests."},
	{Key: "defaults.require_current_target", Type: TypeBoolean, Default: false, Meaning: "Require a PR/MR head to contain the current target tip before final CI evidence and Merge, asking behind Workers to merge the target through a per-target merge queue (ADR 0019, ADR 0031); when false, a green, mergeable PR merges while behind and only conflicts ask the Worker to merge the target.", UserOnly: true},
	{Key: "defaults.pr_poll", Type: TypeDuration, Default: "2m", Meaning: "Minimum interval between pull request and Project checkout polls."},
	{Key: "defaults.review", Type: TypeString, Allowed: []string{"none", "quick", "full-pipeline"}, Default: "none", Meaning: "Review evidence required to Merge a Change Task. full-pipeline validates a private candidate through review, test, docs and lint before every publication.", UserOnly: true},
	{Key: "repositories.<member>.review", Type: TypeString, Allowed: []string{"none", "quick", "full-pipeline"}, Meaning: "Override defaults.review for this workspace Member.", UserOnly: true},
	{Key: "review_loop.enabled", Type: TypeBoolean, Default: true, Meaning: "Enable built-in Review Rounds for Change Tasks.", UserOnly: true},
	{Key: "review_loop.max_rounds", Type: TypeInteger, Default: 3, Meaning: "Maximum Review Rounds per Change Task; may be lowered to 1 or 2.", UserOnly: true},
	{Key: "review_loop.block_at", Type: TypeString, Allowed: []string{"P0", "P1", "P2"}, Default: "P1", Meaning: "Finding severity at or above which introduced findings block Merge.", UserOnly: true},
	{Key: "review_loop.auto_start", Type: TypeBoolean, Default: true, Meaning: "Automatically admit the next Review Round when a completed Task enters the merge flow.", UserOnly: true},
	{Key: "review_loop.docs", Type: TypeBoolean, Default: false, Meaning: "Include documentation gaps in the Review rubric.", UserOnly: true},
	{Key: "review_loop.guidance", Type: TypeStringArray, Default: []string{}, Meaning: "Repository guidance paths supplied to Reviewers from the frozen target branch.", UserOnly: true},
	{Key: "ci_loop.max_fix_rounds", Type: TypeInteger, Default: 3, Meaning: "Maximum automatic CI fix instructions per Change Task; 0 disables them.", UserOnly: true},
	{Key: "ci_loop.rerun_cancelled", Type: TypeInteger, Default: 1, Meaning: "Same-head retries for provider-cancelled checks per check and Change Task.", UserOnly: true},
	{Key: "full_pipeline.run_workflows", Type: TypeBoolean, Default: false, Meaning: "Explicitly allow locally executing repository CI workflow shell steps in the private full-pipeline candidate; unsupported jobs remain visible and cannot be inferred from repository files.", UserOnly: true},
	{Key: "repositories.<repo>.merge_mode", Type: TypeString, Allowed: []string{"local", "pr"}, Meaning: "How this member repository of a workspace Project Merges; defaults to local without an origin, else the Project merge_mode."},
	{Key: "repositories.<repo>.target_branch", Type: TypeString, Default: "", Meaning: "Default PR/MR target branch for this workspace Member; overrides defaults.target_branch and the Member's default branch."},
	{Key: "pool.clean", Type: TypeString, Allowed: []string{"warm", "pristine"}, Default: "warm", Meaning: "Whether released Sandboxes keep ignored files."},
	{Key: "pool.setup", Type: TypeStringArray, Default: []string{}, Meaning: "Commands run in a Sandbox after it is acquired.", UserOnly: true},
	{Key: "pool.keep_idle", Type: TypeInteger, Default: 4, Meaning: "Idle Sandboxes kept for each Project."},
	{Key: "research.native", Type: TypeString, Allowed: []string{ResearchTracked, ResearchOff}, Default: ResearchTracked, Meaning: "Native research children: tracked admits only registered read-only children through hooks and `posse research`; off leaves harness children unmanaged and refuses `posse research start`.", UserOnly: true},
	{Key: "research.max_active", Type: TypeInteger, Default: 2, Meaning: "Maximum registered or running research children per parent (the Lead or one Task)."},
	{Key: "research.max_result_bytes", Type: TypeInteger, Default: 262144, Meaning: "Largest research result kept; longer results are truncated and marked."},
	{Key: "research.max_output_bytes", Type: TypeInteger, Default: 8388608, Meaning: "Largest event stream a process-launched research child may write before Posse stops it."},
	{Key: "research.timeout", Type: TypeDuration, Default: "30m", Meaning: "Longest run of a process-launched research child before Posse stops it."},
	{Key: "retention.task_artifacts", Type: TypeDuration, Default: "2160h", Meaning: "How long Teardown keeps Reports, saved Task attachments and approved discard-tip bundles; `posse pool prune` removes expired artifacts."},
	{Key: "worker_go_cache.max_size", Type: TypeString, Default: "8GiB", Meaning: "Size of the shared Posse Worker GOCACHE at which Posse trims least-recently-used entries; accepts bytes or a KiB, MiB, GiB or TiB suffix. User config only."},
	{Key: "worker_go_cache.min_age", Type: TypeDuration, Default: "6h", Meaning: "Worker GOCACHE entries used more recently are never trimmed, so active Workers keep their builds; at least 2h. User config only."},
	{Key: "worker_go_cache.interval", Type: TypeDuration, Default: "1h", Meaning: "Minimum time between Worker GOCACHE trims run by Project watches and Teardown. User config only."},
	{Key: "kinds.<kind>.disabled", Type: TypeBoolean, Default: false, Meaning: "Disable this kind for new Worker dispatch without removing its historical or frozen launch settings."},
	{Key: "kinds.<kind>.auto_approve_args", Type: TypeStringArray, Default: []string{}, Meaning: "Arguments that enable automatic approvals for this agent kind; Leads also get them by default when lead_auto_approve is true.", UserOnly: true},
	{Key: "kinds.<kind>.lead_auto_approve", Type: TypeBoolean, Default: map[string]bool{"claude": true, "codex": true, "opencode": true}, Meaning: "Whether Leads of this kind also get auto_approve_args; set false to keep the harness's approval prompts. User-only. Pi has no permission system.", UserOnly: true},
	{Key: "kinds.<kind>.model_args", Type: TypeStringArray, Default: []string{}, Meaning: "Argument template for a Profile model; supports {model}.", UserOnly: true},
	{Key: "kinds.<kind>.effort_args", Type: TypeStringArray, Default: []string{}, Meaning: "Argument template for a Profile effort; supports {effort}.", UserOnly: true},
	{Key: "kinds.<kind>.resume_args", Type: TypeStringArray, Default: []string{}, Meaning: "Argument template for resuming a recorded agent session; supports {session}.", UserOnly: true},
	{Key: "kinds.<kind>.system_prompt_args", Type: TypeStringArray, Default: map[string][]string{"claude": {"--append-system-prompt-file", "{file}"}, "codex": {"-c", "developer_instructions={text}"}, "pi": {"--append-system-prompt", "{file}"}}, Meaning: "Argument template that gives the Lead its instructions as a system prompt; supports {file} (the instructions file) and {text} (the complete instructions with whitespace preserved; Codex receives a TOML-quoted string).", UserOnly: true},
	{Key: "kinds.<kind>.lead_args", Type: TypeStringArray, Default: map[string][]string{"codex": {"--sandbox", "danger-full-access"}}, Meaning: "Arguments every Lead of this kind gets so it can run posse commands; codex's default sandbox cannot write the posse home. Approvals are unchanged.", UserOnly: true},
	{Key: "kinds.<kind>.background_commands", Type: TypeBoolean, Default: map[string]bool{"claude": true, "codex": false, "pi": false}, Meaning: "Whether this agent kind can run a background command and be re-invoked when it exits; Workers of such kinds are told to background long commands."},
	{Key: "kinds.<kind>.notice_delivery", Type: TypeString, Allowed: NoticeDeliveries, Default: map[string]string{"claude": NoticeDeliveryWatch, "codex": NoticeDeliveryCodexQueue, "pi": NoticeDeliveryPiExtension, "opencode": NoticeDeliveryOpenCodePlugin, "<other>": NoticeDeliveryPrompt}, Meaning: "How Notices reach a Lead: prompt (typed only when idle and unfocused), watch (Claude-owned bounded Stop hook; background command for other kinds), codex-queue (codex session queue), pi-extension (Pi watch extension), opencode-plugin (Lead-only OpenCode watch and session API)."},
	{Key: "kinds.<kind>.steer", Type: TypeBoolean, Default: map[string]bool{"claude": true, "codex": true, "pi": true}, Meaning: "Allow sending to a working Worker even when focused; unknown kinds default to false."},
	{Key: "kinds.<kind>.interrupt_keys", Type: TypeStringArray, Default: map[string][]string{"claude": {"ctrl+c"}, "codex": {"esc"}, "pi": {"esc"}, "opencode": {"esc"}}, Meaning: "Herdr logical key sequence that aborts a running turn; unknown kinds require an explicit sequence."},
	{Key: "kinds.<kind>.prepare", Type: TypeString, Allowed: []string{"", "claude-trust", "codex-trust"}, Default: "", Meaning: "Built-in preparation step run before starting an agent.", UserOnly: true},
	{Key: "profiles.<profile>.kind", Type: TypeString, Meaning: "Agent kind used by this Profile.", UserOnly: true},
	{Key: "profiles.<profile>.model", Type: TypeString, Default: "", Meaning: "Model argument value supplied through the kind template."},
	{Key: "profiles.<profile>.effort", Type: TypeString, Default: "", Meaning: "Reasoning effort supplied through the kind template."},
	{Key: "profiles.<profile>.args", Type: TypeStringArray, Default: []string{}, Meaning: "Additional arguments passed to this Profile's agent.", UserOnly: true},
	{Key: "profiles.<profile>.attribution", Type: TypeBoolean, Default: false, Meaning: "Keep the agent's commit co-author trailer and PR attribution. When false, Claude launches get attribution disabled and publish refuses commits with agent attribution trailers.", UserOnly: true},
	{Key: "profiles.<profile>.fallback", Type: TypeString, Default: "", Meaning: "Profile that relaunches a Worker in the same Sandbox when this Profile's quota or usage limit runs out; chains may not loop."},
	{Key: "workflows.<workflow>.stages", Type: TypeTableArray, Meaning: "Ordered User-owned workflow stages; a Project replaces a named workflow as a whole.", UserOnly: true},
	{Key: "dispatch", Type: TypeTableArray, Default: []any{}, Meaning: "Ordered dispatch rules that select a Profile."},
	{Key: "dispatch.default.use", Type: TypeString, Default: "", Meaning: "Fallback Profile used when no dispatch rule matches."},
	{Key: "autonomy.yolo", Type: TypeBoolean, Default: false, Meaning: "Shorthand granting the Lead review and merging autonomy.", UserOnly: true},
	{Key: "autonomy.review", Type: TypeString, Allowed: []string{"ask", "lead"}, Default: "ask", Meaning: "Disposition of review findings: whether the Lead may decide without asking the User; never substitutes for review evidence.", UserOnly: true},
	{Key: "autonomy.merge", Type: TypeString, Allowed: []string{"ask", "auto"}, Default: "ask", Meaning: "Whether the Lead may Merge changes without asking the User.", UserOnly: true},
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

func IsRemovedGateKey(key string) bool {
	return key == "gate" || key == "defaults.gate" || key == "gate_loop" || strings.HasPrefix(key, "gate_loop.") ||
		key == "repositories.<repo>.gate" || strings.HasPrefix(key, "repositories.") && strings.HasSuffix(key, ".gate")
}

func IsUserOnly(key string) bool {
	spec, found := Lookup(key)
	return found && spec.UserOnly
}

// userConfigOnlyReason explains why a Project config may not set key, or
// returns "" when it may.
func userConfigOnlyReason(key string) string {
	if key == "worker_go_cache" || strings.HasPrefix(key, "worker_go_cache.") {
		return "worker_go_cache is valid only in the User config"
	}
	return ""
}

func CanonicalProjectKey(key string) string {
	if isProjectScalarAlias(key) {
		return canonicalSchemaKey(key)
	}
	return key
}

func IsUserOnlyFor(key string, projectFile bool) bool {
	if projectFile {
		key = CanonicalProjectKey(key)
	}
	return IsUserOnly(key)
}

func ValidateSetting(key string, value any, projectFile bool) error {
	spec, found := Lookup(key)
	if !found {
		return fmt.Errorf("%s: %s", key, unknownKeyReason(key))
	}
	if strings.HasPrefix(key, "autonomy.") && !projectFile {
		return fmt.Errorf("autonomy is valid only in a Project config")
	}
	if key == "dispatch" {
		if entries, ok := tableEntries(value); ok {
			for _, entry := range entries {
				if replacement := legacyTaskType(entry["type"]); replacement != "" {
					return fmt.Errorf("dispatch.type: %v was removed; use %s", entry["type"], replacement)
				}
			}
		}
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
		if key == "dispatch" {
			valid = dispatchRules(value)
		} else if isWorkflowStagesKey(key) {
			_, valid = workflowTableEntries(value)
		}
	}
	if !valid {
		return fmt.Errorf("%s must be %s", key, spec.Type)
	}
	if text, ok := value.(string); ok {
		if strings.HasSuffix(key, ".notice_delivery") && text == "lookout" {
			return fmt.Errorf("%s: lookout was removed; use watch", key)
		}
		if strings.HasSuffix(key, "auto_teardown") && text == "landed" {
			return fmt.Errorf("%s: landed was removed; use merged", key)
		}
		if key == "defaults.review" || strings.HasPrefix(key, "repositories.") && strings.HasSuffix(key, ".review") {
			if err := ValidateReview(text); err != nil {
				return err
			}
		}
		if key == "defaults.merge_mode" || strings.HasPrefix(key, "repositories.") && strings.HasSuffix(key, ".merge_mode") {
			if err := ValidateMergeMode(text); err != nil {
				return err
			}
		}
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
	if key == "worker_go_cache.max_size" {
		if text, ok := value.(string); ok {
			if _, err := ParseByteSize(text); err != nil {
				return fmt.Errorf("%s %v", key, err)
			}
		}
	}
	if key == "worker_go_cache.min_age" {
		if text, ok := value.(string); ok {
			if parsed, err := time.ParseDuration(text); err == nil && parsed < MinWorkerGoCacheAge {
				return fmt.Errorf("%s must be at least %s", key, MinWorkerGoCacheAge)
			}
		}
	}
	if key == "pool.keep_idle" {
		if number, ok := integerValue(value); ok && number < 0 {
			return fmt.Errorf("%s cannot be negative", key)
		}
	}
	if key == "defaults.max_workers" || key == "defaults.recovery_attempts" {
		if number, ok := integerValue(value); ok && number < 1 {
			return fmt.Errorf("%s must be positive", key)
		}
	}
	if key == "review_loop.max_rounds" {
		if number, ok := integerValue(value); ok && (number < 1 || number > 3) {
			return fmt.Errorf("%s must be between 1 and 3", key)
		}
	}
	if key == "ci_loop.max_fix_rounds" || key == "ci_loop.rerun_cancelled" {
		if number, ok := integerValue(value); ok && number < 0 {
			return fmt.Errorf("%s cannot be negative", key)
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
	issues := SchemaValidationErrors(file, values, projectFile)
	if len(issues) == 0 {
		return nil
	}
	return issues[0]
}

// SchemaValidationErrors reports every independently invalid setting in a
// config map so an edit can distinguish pre-existing errors from new ones.
func SchemaValidationErrors(file string, values map[string]any, projectFile bool) []error {
	issues := workflowSchemaValidationErrors(file, values)
	var walk func(string, any)
	walk = func(prefix string, value any) {
		if replacement := ReplacementKey(prefix); replacement != "" {
			issues = append(issues, &InvalidError{File: file, Key: prefix, Reason: "was removed; use " + replacement})
			return
		}
		switch current := value.(type) {
		case map[string]any:
			if prefix == "dispatch" {
				for _, key := range sortedMapKeys(current) {
					item := current[key]
					if key != "default" {
						issues = append(issues, &InvalidError{File: file, Key: "dispatch." + key, Reason: "unknown configuration key"})
						continue
					}
					walk("dispatch.default", item)
				}
				return
			}
			if !knownContainer(prefix) && prefix != "" {
				issues = append(issues, &InvalidError{File: file, Key: prefix, Reason: "unknown configuration table"})
				return
			}
			for _, key := range sortedMapKeys(current) {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				walk(path, current[key])
			}
		case []map[string]any:
			if isWorkflowStagesKey(prefix) {
				return
			}
			if prefix != "dispatch" {
				issues = append(issues, &InvalidError{File: file, Key: prefix, Reason: prefix + " must be an array of tables"})
				return
			}
			for index, entry := range current {
				for _, key := range sortedMapKeys(entry) {
					path := "dispatch." + key
					if path == "dispatch.default" {
						continue
					}
					if err := validateDispatchValue(file, path, entry[key], index); err != nil {
						issues = append(issues, err)
					}
				}
			}
		case []any:
			if isWorkflowStagesKey(prefix) {
				if _, ok := workflowTableEntries(current); ok {
					return
				}
			}
			key := canonicalSchemaKey(prefix)
			if _, found := Lookup(key); !found {
				issues = append(issues, &InvalidError{File: file, Key: prefix, Reason: "unknown configuration key"})
			} else if err := ValidateSetting(key, current, projectFile); err != nil {
				issues = append(issues, settingIssue(file, key, err))
			}
		default:
			if prefix == "" {
				return
			}
			key := canonicalSchemaKey(prefix)
			if _, found := Lookup(key); !found {
				issues = append(issues, &InvalidError{File: file, Key: prefix, Reason: "unknown configuration key"})
				return
			}
			if err := ValidateSetting(key, value, projectFile); err != nil {
				issues = append(issues, settingIssue(file, key, err))
			}
		}
	}
	if values == nil {
		return nil
	}
	for _, key := range sortedMapKeys(values) {
		if !projectFile && isProjectScalarAlias(key) {
			issues = append(issues, &InvalidError{File: file, Key: key, Reason: "is valid only in a Project config"})
			continue
		}
		walk(key, values[key])
	}
	return uniqueValidationErrors(issues)
}

// settingIssue keeps a MigrationError as the cause so callers can report the
// replacement the User must choose.
func settingIssue(file, key string, err error) *InvalidError {
	var migration *MigrationError
	if errors.As(err, &migration) {
		return &InvalidError{File: file, Key: key, Cause: err}
	}
	return &InvalidError{File: file, Key: key, Reason: strings.TrimPrefix(err.Error(), key+" ")}
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateDispatchValue(file, key string, value any, index int) error {
	valid := false
	switch key {
	case "dispatch.type":
		if replacement := legacyTaskType(value); replacement != "" {
			return &InvalidError{File: file, Key: key, Reason: fmt.Sprintf("%v was removed; use %s", value, replacement)}
		}
		text, ok := value.(string)
		valid = ok && oneOf(text, "change", "research", "review")
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
	case "max_workers", "stall_after", "idle_after", "auto_teardown", "merge_mode", "merge_method", "forge", "pr_poll", "review":
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
	case "merge_mode", "max_workers", "stall_after", "idle_after", "auto_teardown", "merge_method", "forge", "pr_poll", "review":
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
