package app

import (
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func configuredLaunchIdentity(profileName string, profile config.Profile, kind config.Kind, args []string) store.TaskLaunchIdentity {
	model, known := configuredModelFromLaunchArgs(profile.Model, profile.Kind, args)
	return store.TaskLaunchIdentity{Profile: profileName, ConfiguredModel: model, ModelKnown: known}
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

// reviewSessionFailure refuses a Review launch whose arguments could resume an
// existing agent session instead of starting the Review Task's own.
func reviewSessionFailure(profile, refusal string) error {
	return axi.Failure("review_identity_ineligible", refusal, false,
		"Review launch configuration refused for Profile "+profile+": "+refusal,
		"A Review Task runs in its own fresh agent session; remove session selectors and unclassified options from the Profile and kind arguments")
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
		"plan_revision":    identity.PlanRevision,
		"profile":          identity.Profile,
		"configured_model": identity.ConfiguredModel,
		"model_known":      identity.ModelKnown,
	}
}
