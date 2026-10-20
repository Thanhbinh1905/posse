package config

import "fmt"

// LegacyReview identifies stored pre-policy plans, never a configuration alias.
func LegacyReview(value string) bool {
	return value == "always" || value == "on_risk" || value == "never"
}

func ValidateReview(value string) error {
	if LegacyReview(value) {
		return fmt.Errorf("legacy review policy %q is no longer supported; explicitly choose none, quick, full-pipeline or no-mistakes (default none); replace the named key with `posse config set <key> <policy>` or remove it with `posse config unset <key>` to inherit the default; add `--project <name>` for Project config and `--user-approved` from a Lead; there is no automatic migration", value)
	}
	if value == "local" {
		return fmt.Errorf("review policy %q is no longer supported; use quick", value)
	}
	if value != "none" && value != "quick" && value != "full-pipeline" && value != "no-mistakes" {
		return fmt.Errorf("must be none, quick, full-pipeline or no-mistakes")
	}
	return nil
}

// ValidateHistoricalReview accepts values frozen into plans before the review
// policy rename. It is not used for new config or Brief input.
func ValidateHistoricalReview(value string) error {
	if value != "none" && value != "local" && value != "no-mistakes" {
		return fmt.Errorf("unsupported historical review policy %q", value)
	}
	return nil
}

func (c Config) MemberReview(name string) string {
	if review := c.Repositories[name].Review; review != "" {
		return review
	}
	return c.Defaults.Review
}
