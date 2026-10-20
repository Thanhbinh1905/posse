package config

import "fmt"

// LegacyReview identifies stored pre-policy plans, never a configuration alias.
func LegacyReview(value string) bool {
	return value == "always" || value == "on_risk" || value == "never"
}

func ValidateReview(value string) error {
	if LegacyReview(value) {
		return fmt.Errorf("legacy review policy %q is no longer supported; explicitly choose none, quick, full-pipeline, local or no-mistakes (default none); replace the named key with `posse config set <key> <policy>` or remove it with `posse config unset <key>` to inherit the default; add `--project <name>` for Project config and `--user-approved` from a Lead; there is no automatic migration", value)
	}
	if value != "none" && value != "quick" && value != "full-pipeline" && value != "local" && value != "no-mistakes" {
		return fmt.Errorf("must be none, quick, full-pipeline, local or no-mistakes")
	}
	return nil
}

func (c Config) MemberReview(name string) string {
	if review := c.Repositories[name].Review; review != "" {
		return review
	}
	return c.Defaults.Review
}
