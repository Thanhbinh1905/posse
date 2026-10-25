package app

import (
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

// usesRemovedNoMistakes reports a Task admitted under the removed no-mistakes
// Merge Mode or review policy. Its stored plan and review receipts stay
// readable, but this release can neither deliver nor review it.
func usesRemovedNoMistakes(task store.Task) bool {
	if task.MergeMode == config.NoMistakes {
		return true
	}
	plan, err := task.AdmittedPlan()
	if err != nil || plan == nil {
		return false
	}
	if plan.Review == config.NoMistakes {
		return true
	}
	for _, member := range plan.Execution.Members {
		if member.Review == config.NoMistakes {
			return true
		}
	}
	return false
}

func noMistakesRemoved(task store.Task) error {
	id := taskIDString(task.Seq)
	return axi.Failure("no_mistakes_removed", "Task "+id+" was admitted with the removed no-mistakes Merge Mode or review policy; this release cannot deliver, review or merge it", false,
		"Cancel it with `posse teardown "+id+" --discard --user-approved \"<User's words>\"`, then start a replacement Task under pr delivery with full-pipeline, quick or none review",
		"To finish it unchanged instead, reinstall the previous Posse release; `posse update` refuses this release while such a Task is live")
}

// deliveredByPullRequest reports whether a Task's Merge Mode published a pull
// request. Tasks admitted under the removed no-mistakes mode published one too,
// so cleanup of their branches and Sandboxes follows pull request rules.
func deliveredByPullRequest(mode string) bool {
	return mode == "pr" || mode == config.NoMistakes
}
