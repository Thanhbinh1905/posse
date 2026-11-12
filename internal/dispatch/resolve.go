package dispatch

import (
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/config"
)

type Resolution struct {
	Profile    string
	Rule       string
	Candidates []Candidate
	Resolved   config.Profile
}

type Candidate struct {
	When    string
	Profile string
	Scope   string
	Source  string
	// Default marks a Profile that applies when no `when` condition does.
	Default bool
}

type ProfileError struct {
	Code       string
	Message    string
	Candidates []Candidate
}

func (e *ProfileError) Error() string { return e.Message }

const (
	// Keep the kind-only marker stable for existing Task readers.
	ImplicitRule            = "implicit: lead kind"
	ImplicitLeadProfileRule = "implicit: lead profile"
)

// StoredProfile reconstructs an implicit Profile from the Task record, not the
// current Lead. Its kind survives config changes and server recovery. A
// disabled kind remains readable here so existing Tasks retain their launch.
func StoredProfile(cfg config.Config, name, rule string) (config.Profile, bool) {
	if (rule == ImplicitRule || rule == ImplicitLeadProfileRule) && strings.HasPrefix(name, "implicit:") {
		kind := strings.TrimPrefix(name, "implicit:")
		_, ok := cfg.Kinds[kind]
		return config.Profile{Kind: kind}, ok
	}
	profile, ok := cfg.Profiles[name]
	return profile, ok
}

// Precedence is the order Resolve applies. It is the only definition of
// dispatch precedence; `posse config show --effective` prints it and
// docs/spec.md section 11 documents it.
var Precedence = []string{
	"1. --profile <name> wins.",
	"2. For a Review Task, review_loop.profile.",
	"3. Rules apply when their type equals the Task type or they have no type, from the Project and the global config alike.",
	"4. The default tier is the first non-empty one of: unconditional rules of the Task type, unconditional rules with no type, dispatch.default (the Project value replaces the global one).",
	"5. Posse never evaluates `when`. With no applicable conditional rule, a default tier of one rule naming one Profile is used.",
	"6. Otherwise profile_required lists the conditional candidates in config order (Project before global), then the default tier marked default. The Lead passes --profile.",
	"7. With no Dispatch Rules and no dispatch.default, the Profile is inherited from the Lead per defaults.dispatch_inheritance.",
}

func RulePrecedence(rule config.Rule) string {
	switch {
	case rule.When != "":
		return "conditional: listed for the Lead to choose"
	case rule.Type != "":
		return "default for type=" + rule.Type
	default:
		return "default for types with no unconditional typed rule"
	}
}

// Resolve chooses the Profile for a new Task in the order of Precedence.
func Resolve(cfg config.Config, taskType, requested string) (Resolution, error) {
	resolve := func(name, rule string) (Resolution, error) {
		profile, found := cfg.Profiles[name]
		if !found {
			return Resolution{}, &ProfileError{Code: "profile_unknown", Message: fmt.Sprintf("unknown Profile %q", name)}
		}
		if kind := cfg.Kinds[profile.Kind]; kind.Disabled {
			return Resolution{}, disabledKindError(profile.Kind, name)
		}
		return Resolution{Profile: name, Rule: rule, Resolved: profile}, nil
	}
	if requested != "" {
		return resolve(requested, "--profile "+requested)
	}
	if taskType == "review" && cfg.ReviewLoop.Profile != "" {
		return resolve(cfg.ReviewLoop.Profile, "review_loop.profile")
	}
	var conditional []Candidate
	var typedDefaults, genericDefaults []config.Rule
	for _, rule := range cfg.Dispatch {
		if rule.Type != taskType && rule.Type != "" {
			continue
		}
		switch {
		case rule.When != "":
			for _, name := range rule.ProfileNames() {
				conditional = append(conditional, Candidate{When: rule.When, Profile: name, Scope: ruleScope(rule), Source: rule.Source})
			}
		case rule.Type != "":
			typedDefaults = append(typedDefaults, rule)
		default:
			genericDefaults = append(genericDefaults, rule)
		}
	}
	defaultRules := typedDefaults
	if len(defaultRules) == 0 {
		defaultRules = genericDefaults
	}
	var defaults []Candidate
	for _, rule := range defaultRules {
		for _, name := range rule.ProfileNames() {
			defaults = append(defaults, Candidate{Profile: name, Scope: ruleScope(rule), Source: rule.Source, Default: true})
		}
	}
	if len(conditional) == 0 {
		if len(defaultRules) == 1 && len(defaults) == 1 {
			return resolve(defaults[0].Profile, ruleDescription(defaultRules[0]))
		}
		if len(defaults) == 0 && cfg.DispatchDefault.Use != "" {
			return resolve(cfg.DispatchDefault.Use, "dispatch.default")
		}
	}
	if len(defaults) == 0 && cfg.DispatchDefault.Use != "" {
		defaults = []Candidate{{Profile: cfg.DispatchDefault.Use, Scope: "dispatch.default", Source: cfg.DispatchDefault.Source, Default: true}}
	}
	if candidates := append(conditional, defaults...); len(candidates) > 0 {
		return Resolution{}, &ProfileError{Code: "profile_required", Message: "multiple Dispatch Rules could apply; choose a Profile", Candidates: candidates}
	}
	if len(cfg.Dispatch) == 0 && cfg.Lead.Kind != "" {
		if _, ok := cfg.Kinds[cfg.Lead.Kind]; ok {
			rule := ImplicitRule
			if cfg.Defaults.DispatchInheritance == "lead_profile" {
				rule = ImplicitLeadProfileRule
				if profileName := cfg.Lead.Profiles[cfg.Lead.Kind]; profileName != "" {
					return resolve(profileName, rule)
				}
			}
			if cfg.Kinds[cfg.Lead.Kind].Disabled {
				return Resolution{}, disabledKindError(cfg.Lead.Kind, "")
			}
			return Resolution{Profile: "implicit:" + cfg.Lead.Kind, Rule: rule, Resolved: config.Profile{Kind: cfg.Lead.Kind}}, nil
		}
	}
	return Resolution{}, &ProfileError{Code: "profile_required", Message: "no Dispatch Rule matches and dispatch.default.use is not configured"}
}

func disabledKindError(kind, profile string) *ProfileError {
	message := fmt.Sprintf("agent kind %q is disabled for new dispatch", kind)
	if profile != "" {
		message = fmt.Sprintf("Profile %q uses agent kind %q, which is disabled for new dispatch", profile, kind)
	}
	return &ProfileError{
		Code:    "kind_disabled",
		Message: message,
	}
}

func ruleDescription(rule config.Rule) string {
	if rule.Type != "" {
		return "type=" + rule.Type
	}
	return "when=" + rule.When
}

func ruleScope(rule config.Rule) string {
	if rule.Type != "" {
		return "type=" + rule.Type
	}
	return "type=any"
}
