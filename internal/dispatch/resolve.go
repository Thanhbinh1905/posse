package dispatch

import (
	"fmt"

	"github.com/thanhbinh1905/posse/internal/config"
)

type Resolution struct {
	Profile    string
	Rule       string
	Candidates []Candidate
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

func Resolve(cfg config.Config, taskType, requested string) (Resolution, error) {
	if requested != "" {
		if _, found := cfg.Profiles[requested]; !found {
			return Resolution{}, &ProfileError{Code: "profile_unknown", Message: fmt.Sprintf("unknown Profile %q", requested)}
		}
		return Resolution{Profile: requested, Rule: "--profile " + requested}, nil
	}
	var matches []config.Rule
	for _, rule := range cfg.Dispatch {
		if rule.Type == "" || rule.Type == taskType {
			matches = append(matches, rule)
		}
	}
	if len(matches) == 1 && matches[0].When == "" {
		return Resolution{Profile: matches[0].Use, Rule: ruleDescription(matches[0])}, nil
	}
	if len(matches) > 0 {
		candidates := make([]Candidate, len(matches))
		for i, rule := range matches {
			candidates[i] = Candidate{When: rule.When, Profile: rule.Use}
		}
		return Resolution{}, &ProfileError{Code: "profile_required", Message: "multiple Dispatch Rules could apply; choose a Profile", Candidates: candidates}
	}
	if cfg.DispatchDefault.Use == "" {
		return Resolution{}, &ProfileError{Code: "profile_required", Message: "no Dispatch Rule matches and dispatch.default.use is not configured"}
	}
	return Resolution{Profile: cfg.DispatchDefault.Use, Rule: "dispatch.default"}, nil
}

func ruleDescription(rule config.Rule) string {
	if rule.Type != "" {
		return "type=" + rule.Type
	}
	return "when=" + rule.When
}
