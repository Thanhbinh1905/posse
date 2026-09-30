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

const ImplicitRule = "implicit: lead kind"

// StoredProfile reconstructs an implicit Profile from the Task record, not the
// current Lead. Its kind survives config changes and server recovery.
func StoredProfile(cfg config.Config, name, rule string) (config.Profile, bool) {
	if rule == ImplicitRule && strings.HasPrefix(name, "implicit:") {
		kind := strings.TrimPrefix(name, "implicit:")
		_, ok := cfg.Kinds[kind]
		return config.Profile{Kind: kind}, ok
	}
	profile, ok := cfg.Profiles[name]
	return profile, ok
}

func Resolve(cfg config.Config, taskType, requested string) (Resolution, error) {
	resolve := func(name, rule string) Resolution {
		return Resolution{Profile: name, Rule: rule, Resolved: cfg.Profiles[name]}
	}
	if requested != "" {
		if _, found := cfg.Profiles[requested]; !found {
			return Resolution{}, &ProfileError{Code: "profile_unknown", Message: fmt.Sprintf("unknown Profile %q", requested)}
		}
		return resolve(requested, "--profile "+requested), nil
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
			return resolve(defaults[0].Use, ruleDescription(defaults[0])), nil
		}
	}
	if len(matches) == 1 && matches[0].When == "" {
		return resolve(matches[0].Use, ruleDescription(matches[0])), nil
	}
	if len(matches) > 0 {
		candidates := make([]Candidate, len(matches))
		for i, rule := range matches {
			candidates[i] = Candidate{When: rule.When, Profile: rule.Use}
		}
		return Resolution{}, &ProfileError{Code: "profile_required", Message: "multiple Dispatch Rules could apply; choose a Profile", Candidates: candidates}
	}
	if cfg.DispatchDefault.Use != "" {
		return resolve(cfg.DispatchDefault.Use, "dispatch.default"), nil
	}
	if len(cfg.Dispatch) == 0 && cfg.Lead.Kind != "" {
		if _, ok := cfg.Kinds[cfg.Lead.Kind]; ok {
			return Resolution{Profile: "implicit:" + cfg.Lead.Kind, Rule: ImplicitRule, Resolved: config.Profile{Kind: cfg.Lead.Kind}}, nil
		}
	}
	return Resolution{}, &ProfileError{Code: "profile_required", Message: "no Dispatch Rule matches and dispatch.default.use is not configured"}
}

func ruleDescription(rule config.Rule) string {
	if rule.Type != "" {
		return "type=" + rule.Type
	}
	return "when=" + rule.When
}
