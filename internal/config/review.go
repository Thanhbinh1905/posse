package config

import "fmt"

// LegacyReview identifies stored pre-policy plans, never a configuration alias.
func LegacyReview(value string) bool {
	return value == "always" || value == "on_risk" || value == "never"
}

// NoMistakes is the removed Merge Mode and review policy. Stored plans and
// review receipts written before its removal still name it.
const NoMistakes = "no-mistakes"

// MigrationError reports a removed setting value together with the replacement
// the User must choose; Posse never rewrites it automatically.
type MigrationError struct {
	Value       string
	Replacement string
	Lighter     []string
}

func (e *MigrationError) Error() string {
	message := fmt.Sprintf("%s was removed; use %s", e.Value, e.Replacement)
	if len(e.Lighter) == 2 {
		message += fmt.Sprintf(" (%s or %s are lighter choices)", e.Lighter[0], e.Lighter[1])
	}
	return message + "; built-in validation replaces no-mistakes (docs/spec.md section 25)"
}

func ValidateReview(value string) error {
	if LegacyReview(value) {
		return fmt.Errorf("legacy review policy %q is no longer supported; explicitly choose none, quick or full-pipeline (default none); replace the named key with `posse config set <key> <policy>` or remove it with `posse config unset <key>` to inherit the default; add `--project <name>` for Project config and `--user-approved` from a Lead; there is no automatic migration", value)
	}
	if value == "local" {
		return fmt.Errorf("review policy %q is no longer supported; use quick", value)
	}
	if value == NoMistakes {
		return &MigrationError{Value: value, Replacement: "full-pipeline", Lighter: []string{"quick", "none"}}
	}
	if value != "none" && value != "quick" && value != "full-pipeline" {
		return fmt.Errorf("must be none, quick or full-pipeline")
	}
	return nil
}

// ValidateMergeMode accepts the Merge Modes new config and Briefs may select.
func ValidateMergeMode(value string) error {
	if value == NoMistakes {
		return &MigrationError{Value: value, Replacement: "pr"}
	}
	if value != "local" && value != "pr" {
		return fmt.Errorf("must be local or pr")
	}
	return nil
}

// ValidateHistoricalReview accepts values frozen into plans before the review
// policy rename. It is not used for new config or Brief input.
func ValidateHistoricalReview(value string) error {
	if value != "none" && value != "local" && value != NoMistakes {
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
