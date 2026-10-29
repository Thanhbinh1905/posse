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
	var typedMatches, genericMatches []config.Rule
	for _, rule := range cfg.Dispatch {
		if rule.Type == taskType {
			typedMatches = append(typedMatches, rule)
		} else if rule.Type == "" {
			genericMatches = append(genericMatches, rule)
		}
	}
	matches := genericMatches
	if len(typedMatches) > 0 {
		// Exact-type rules exclude generic fallbacks. A unique typed default
		// wins over conditional alternatives, which the Lead selects via --profile.
		matches = typedMatches
		var defaults []config.Rule
		for _, rule := range typedMatches {
			if rule.When == "" {
				defaults = append(defaults, rule)
			}
		}
		if len(defaults) == 1 {
			return resolve(defaults[0].Use, ruleDescription(defaults[0]))
		}
	}
	if len(matches) == 1 && matches[0].When == "" {
		return resolve(matches[0].Use, ruleDescription(matches[0]))
	}
	if len(matches) > 0 {
		candidates := make([]Candidate, len(matches))
		for i, rule := range matches {
			candidates[i] = Candidate{When: rule.When, Profile: rule.Use}
		}
		return Resolution{}, &ProfileError{Code: "profile_required", Message: "multiple Dispatch Rules could apply; choose a Profile", Candidates: candidates}
	}
	if cfg.DispatchDefault.Use != "" {
		return resolve(cfg.DispatchDefault.Use, "dispatch.default")
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
