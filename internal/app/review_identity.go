package app

import (
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

type reviewIdentityCheck struct {
	Eligible bool
	Reason   string
}

func configuredLaunchIdentity(profileName string, profile config.Profile, kind config.Kind) store.TaskLaunchIdentity {
	model, known := configuredModelIdentity(profile, kind)
	return store.TaskLaunchIdentity{Profile: profileName, ConfiguredModel: model, ModelKnown: known}
}

// configuredModelIdentity records only the configured Profile model. Any
// additional model-setting arguments make the effective configured identity
// unknown rather than relying on harness-specific flag parsing.
func configuredModelIdentity(profile config.Profile, kind config.Kind) (string, bool) {
	if strings.TrimSpace(profile.Model) == "" || !modelTemplateUsesOneValue(kind.ModelArgs) {
		return profile.Model, false
	}
	if hasModelOverride(profile.Args) || hasModelOverride(kind.AutoApproveArgs) || hasModelOverride(kind.EffortArgs) {
		return profile.Model, false
	}
	return profile.Model, true
}

func modelTemplateUsesOneValue(templates []string) bool {
	if strings.Count(strings.Join(templates, "\x00"), "{model}") != 1 {
		return false
	}
	for index, template := range templates {
		if !isModelOption(template) && !containsModelAssignment(template) {
			continue
		}
		if strings.Contains(template, "{model}") || (index+1 < len(templates) && templates[index+1] == "{model}") {
			continue
		}
		return false
	}
	return true
}

func hasModelOverride(args []string) bool {
	for _, arg := range args {
		if isModelOption(arg) || containsModelAssignment(arg) {
			return true
		}
	}
	return false
}

func isModelOption(arg string) bool {
	option := strings.ToLower(strings.TrimSpace(arg))
	return option == "--model" || strings.HasPrefix(option, "--model-") || strings.HasPrefix(option, "--model_") || strings.HasPrefix(option, "--model=") || option == "-m" || strings.HasPrefix(option, "-m=") || (len(option) > 2 && strings.HasPrefix(option, "-m"))
}

func containsModelAssignment(value string) bool {
	for remaining := strings.ToLower(value); ; {
		index := strings.Index(remaining, "model")
		if index < 0 {
			return false
		}
		beforeIsWord := index > 0 && (isArgumentWord(remaining[index-1]))
		end := index + len("model")
		afterIsWord := end < len(remaining) && isArgumentWord(remaining[end])
		if !beforeIsWord && !afterIsWord {
			for end < len(remaining) && (remaining[end] == ' ' || remaining[end] == '\t') {
				end++
			}
			if end < len(remaining) && remaining[end] == '=' {
				return true
			}
		}
		remaining = remaining[index+len("model"):]
	}
}

func isArgumentWord(r byte) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

func reviewSessionOverrideReason(kind string, args []string) string {
	for index, arg := range args {
		value := strings.ToLower(strings.TrimSpace(arg))
		option := value
		if before, _, found := strings.Cut(value, "="); found {
			option = strings.TrimSpace(before)
		}
		if isReviewSessionOption(option, kind) || value == "resume" || value == "fork" || value == "continue" {
			return fmt.Sprintf("launch arguments contain session-selection option %q", arg)
		}
		if containsSessionAssignment(value) {
			return fmt.Sprintf("launch arguments contain a session or thread identity in %q", arg)
		}
		if kind == "codex" && (option == "-c" || option == "--config") && index+1 < len(args) && containsSessionAssignment(args[index+1]) {
			return fmt.Sprintf("launch arguments configure a session or thread identity in %q", args[index+1])
		}
	}
	return ""
}

func isReviewSessionOption(option, kind string) bool {
	switch option {
	case "--resume", "-r", "--continue", "--fork", "--fork-session", "--session", "--session-id", "--session_id", "--thread", "--thread-id", "--thread_id", "--conversation", "--conversation-id", "--conversation_id", "--teleport", "--from-pr", "--cloud":
		return true
	case "-c":
		return kind != "codex"
	case "-s":
		return kind != "codex" && kind != "claude"
	default:
		return strings.HasPrefix(option, "--resume-") || strings.HasPrefix(option, "--session-") || strings.HasPrefix(option, "--session_") || strings.HasPrefix(option, "--thread-") || strings.HasPrefix(option, "--thread_") || strings.HasPrefix(option, "--conversation-") || strings.HasPrefix(option, "--conversation_")
	}
}

func containsSessionAssignment(value string) bool {
	value = strings.ToLower(value)
	for _, key := range []string{"session", "session_id", "session-id", "thread", "thread_id", "thread-id", "conversation", "conversation_id", "conversation-id"} {
		for remaining := value; ; {
			index := strings.Index(remaining, key)
			if index < 0 {
				break
			}
			beforeIsWord := index > 0 && isArgumentWord(remaining[index-1])
			end := index + len(key)
			afterIsWord := end < len(remaining) && isArgumentWord(remaining[end])
			if !beforeIsWord && !afterIsWord {
				for end < len(remaining) && (remaining[end] == ' ' || remaining[end] == '\t') {
					end++
				}
				if end < len(remaining) && remaining[end] == '=' {
					return true
				}
			}
			remaining = remaining[index+len(key):]
		}
	}
	return false
}

// reviewResumeArgsUseOwnSession accepts an explicit selector whose value is
// Posse's {session} placeholder. Continuation and literal-session selectors
// are not safe for a Review Task relaunch.
func reviewResumeArgsUseOwnSession(args []string) bool {
	if len(args) == 0 {
		return true
	}
	if strings.Count(strings.Join(args, "\x00"), "{session}") != 1 {
		return false
	}
	selectors := 0
	for index, arg := range args {
		value := strings.ToLower(strings.TrimSpace(arg))
		option := value
		if before, _, found := strings.Cut(value, "="); found {
			option = strings.TrimSpace(before)
		}
		switch option {
		case "resume":
			if index+1 < len(args) && args[index+1] == "{session}" {
				selectors++
			} else {
				return false
			}
		case "--resume", "-r", "--session", "--session-id", "--session_id", "--thread", "--thread-id", "--thread_id", "--conversation", "--conversation-id", "--conversation_id":
			if strings.Contains(arg, "{session}") {
				selectors++
			} else if index+1 < len(args) && args[index+1] == "{session}" {
				selectors++
			} else {
				return false
			}
		case "--continue", "-c", "--fork", "--fork-session", "--teleport", "--from-pr", "--cloud":
			return false
		}
	}
	return selectors == 1
}

func reviewResumeArgsRefusal(args []string, session string) string {
	if session == "" || len(args) == 0 {
		return ""
	}
	if !reviewResumeArgsUseOwnSession(args) {
		return "configured resume arguments do not select only Posse's own Review Task session"
	}
	if hasModelOverride(args) {
		return "configured resume arguments can override the Review Task model"
	}
	return ""
}

func launchHistoryComplete(task store.Task, identities []store.TaskLaunchIdentity) bool {
	if task.Launches == 0 {
		return len(identities) == 0
	}
	if task.Launches < 0 || len(identities) != task.Launches {
		return false
	}
	for index, identity := range identities {
		if identity.TaskID != task.ID || identity.LaunchNumber != index+1 || identity.Profile == "" {
			return false
		}
	}
	return true
}

func compareReviewIdentity(authorLaunches []store.TaskLaunchIdentity, authorHistoryKnown bool, reviewer store.TaskLaunchIdentity) reviewIdentityCheck {
	var reasons []string
	if reviewer.Profile == "" {
		reasons = append(reasons, "the Review Task Profile is unknown")
	}
	if !reviewer.ModelKnown || strings.TrimSpace(reviewer.ConfiguredModel) == "" {
		reasons = append(reasons, "the Review Task has no determinable configured model")
	}
	if !authorHistoryKnown {
		reasons = append(reasons, "the Ship Task launch-identity history is incomplete")
	}
	for _, launch := range authorLaunches {
		if launch.Profile == "" {
			reasons = append(reasons, fmt.Sprintf("author launch %d has an unknown Profile", launch.LaunchNumber))
		} else if reviewer.Profile != "" && reviewer.Profile == launch.Profile {
			reasons = append(reasons, fmt.Sprintf("Profile matches author launch %d", launch.LaunchNumber))
		}
		if !launch.ModelKnown || strings.TrimSpace(launch.ConfiguredModel) == "" {
			reasons = append(reasons, fmt.Sprintf("author launch %d has an unknown configured model", launch.LaunchNumber))
		} else if reviewer.ModelKnown && reviewer.ConfiguredModel == launch.ConfiguredModel {
			reasons = append(reasons, fmt.Sprintf("configured model matches author launch %d", launch.LaunchNumber))
		}
	}
	return reviewIdentityCheck{Eligible: len(reasons) == 0, Reason: strings.Join(reasons, "; ")}
}

func identityModelText(identity store.TaskLaunchIdentity) string {
	if !identity.ModelKnown || strings.TrimSpace(identity.ConfiguredModel) == "" {
		return "<unknown>"
	}
	return fmt.Sprintf("%q", identity.ConfiguredModel)
}

func reviewIdentityDescription(identity store.TaskLaunchIdentity) string {
	profile := identity.Profile
	if profile == "" {
		profile = "<unknown>"
	}
	return fmt.Sprintf("Profile=%s model=%s", profile, identityModelText(identity))
}

func reviewIdentityFailure(author store.Task, authorLaunches []store.TaskLaunchIdentity, authorHistoryKnown bool, reviewer store.TaskLaunchIdentity, launchRefusal ...string) error {
	check := compareReviewIdentity(authorLaunches, authorHistoryKnown, reviewer)
	reasons := []string{check.Reason}
	help := []string{"Configured reviewer identity: " + reviewIdentityDescription(reviewer)}
	if len(launchRefusal) > 0 && launchRefusal[0] != "" {
		reasons = append(reasons, launchRefusal[0])
		help = append(help, "Review launch configuration refused: "+launchRefusal[0])
	}
	for _, launch := range authorLaunches {
		help = append(help, fmt.Sprintf("Author %s launch %d: %s", taskIDString(author.Seq), launch.LaunchNumber, reviewIdentityDescription(launch)))
	}
	if !authorHistoryKnown {
		help = append(help, "The author launch history is incomplete or unavailable; Posse will not infer missing identities")
	}
	help = append(help, "Choose a fresh Review Task Profile with a configured model that differs from every author launch")
	return axi.Failure("review_identity_ineligible", strings.Trim(strings.Join(reasons, "; "), "; "), false, help...)
}

func reviewRelaunchIdentityFailure(author store.Task, authorLaunches []store.TaskLaunchIdentity, authorHistoryKnown bool, reviewer store.Task, reviewerLaunches []store.TaskLaunchIdentity, reviewerHistoryKnown bool, current store.TaskLaunchIdentity, launchRefusal ...string) error {
	var reasons []string
	if !authorHistoryKnown {
		reasons = append(reasons, "the Ship Task launch-identity history is incomplete")
	}
	if !reviewerHistoryKnown {
		reasons = append(reasons, "the existing Review Task launch-identity history is incomplete; a fresh Review Task/session is required")
	}
	for _, prior := range reviewerLaunches {
		check := compareReviewIdentity(authorLaunches, authorHistoryKnown, prior)
		if !check.Eligible {
			reasons = append(reasons, fmt.Sprintf("existing Review Task launch %d is not independent: %s", prior.LaunchNumber, check.Reason))
		}
	}
	currentCheck := compareReviewIdentity(authorLaunches, authorHistoryKnown, current)
	if !currentCheck.Eligible {
		reasons = append(reasons, "new launch is not independent: "+currentCheck.Reason)
	}
	help := []string{"Configured next reviewer identity: " + reviewIdentityDescription(current)}
	if len(launchRefusal) > 0 && launchRefusal[0] != "" {
		reasons = append(reasons, launchRefusal[0])
		help = append(help, "Review launch configuration refused: "+launchRefusal[0])
	}
	for _, prior := range reviewerLaunches {
		help = append(help, fmt.Sprintf("Review %s launch %d: %s", taskIDString(reviewer.Seq), prior.LaunchNumber, reviewIdentityDescription(prior)))
	}
	for _, launch := range authorLaunches {
		help = append(help, fmt.Sprintf("Author %s launch %d: %s", taskIDString(author.Seq), launch.LaunchNumber, reviewIdentityDescription(launch)))
	}
	help = append(help, "Start a fresh Review Task/session if any earlier reviewer launch was ineligible")
	return axi.Failure("review_identity_ineligible", strings.Join(reasons, "; "), false, help...)
}

func reviewIdentityResult(author store.Task, authorLaunches []store.TaskLaunchIdentity, authorHistoryKnown bool, reviewer store.TaskLaunchIdentity) map[string]any {
	check := compareReviewIdentity(authorLaunches, authorHistoryKnown, reviewer)
	return map[string]any{
		"author_task":          taskIDString(author.Seq),
		"eligible":             check.Eligible,
		"reason":               check.Reason,
		"reviewer":             identityView(reviewer),
		"author_launches":      identityViews(authorLaunches),
		"author_history_known": authorHistoryKnown,
	}
}

func reviewIdentityHistoryResult(author store.Task, authorLaunches []store.TaskLaunchIdentity, authorHistoryKnown bool, reviewer store.Task, reviewerLaunches []store.TaskLaunchIdentity, reviewerHistoryKnown bool) map[string]any {
	comparisons := make([]any, 0, len(reviewerLaunches))
	var reasons []string
	if !authorHistoryKnown {
		reasons = append(reasons, "the Ship Task launch-identity history is incomplete")
	}
	if !reviewerHistoryKnown {
		reasons = append(reasons, "the Review Task launch-identity history is incomplete")
	}
	if len(reviewerLaunches) == 0 {
		reasons = append(reasons, "the Review Task has no recorded launch identity")
	}
	for _, identity := range reviewerLaunches {
		check := compareReviewIdentity(authorLaunches, authorHistoryKnown, identity)
		comparisons = append(comparisons, map[string]any{"review_launch": identity.LaunchNumber, "eligible": check.Eligible, "reason": check.Reason, "identity": identityView(identity)})
		if !check.Eligible {
			reasons = append(reasons, fmt.Sprintf("Review Task launch %d: %s", identity.LaunchNumber, check.Reason))
		}
	}
	return map[string]any{
		"author_task":          taskIDString(author.Seq),
		"review_task":          taskIDString(reviewer.Seq),
		"eligible":             len(reasons) == 0,
		"reason":               strings.Join(reasons, "; "),
		"author_history_known": authorHistoryKnown,
		"review_history_known": reviewerHistoryKnown,
		"author_launches":      identityViews(authorLaunches),
		"review_launches":      comparisons,
	}
}

func identityViews(identities []store.TaskLaunchIdentity) []any {
	views := make([]any, 0, len(identities))
	for _, identity := range identities {
		views = append(views, identityView(identity))
	}
	return views
}

func identityView(identity store.TaskLaunchIdentity) map[string]any {
	return map[string]any{
		"launch_number":    identity.LaunchNumber,
		"profile":          identity.Profile,
		"configured_model": identity.ConfiguredModel,
		"model_known":      identity.ModelKnown,
	}
}
