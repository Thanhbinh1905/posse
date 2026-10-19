package store

import "strings"

func currentTaskType(value string) string {
	switch value {
	case "ship":
		return "change"
	case "scout":
		return "research"
	}
	return value
}

func currentTaskState(value string) string {
	switch value {
	case "landing":
		return "merging"
	case "landed":
		return "merged"
	}
	return value
}

func currentNoticeKind(value string) string {
	for _, pair := range [][2]string{{"rider_", "worker_"}, {"mount_", "sandbox_"}, {"lookout_", "watch_"}, {"land_", "merge_"}, {"unsaddle_", "teardown_"}} {
		if strings.HasPrefix(value, pair[0]) {
			return pair[1] + strings.TrimPrefix(value, pair[0])
		}
	}
	switch value {
	case "leftover":
		return "orphan"
	case "remuda_orphan":
		return "pool_orphan"
	case "recovery_lookout_reattached":
		return "recovery_watch_reattached"
	case "leftover_transferred":
		return "orphan_transferred"
	case "leftover_discarded":
		return "orphan_discarded"
	}
	return value
}

func normalizeProjectSnapshotVocabulary(state *projectStateSnapshot) {
	for i := range state.Notices {
		state.Notices[i].Kind = currentNoticeKind(state.Notices[i].Kind)
	}
	for i := range state.Decisions {
		decision := &state.Decisions[i]
		decision.Kind = currentNoticeKind(decision.Kind)
		if strings.HasPrefix(decision.Origin, "leftover:") {
			decision.Origin = "orphan:" + strings.TrimPrefix(decision.Origin, "leftover:")
		}
		// Only this machine-defined option denotes integration. A Worker's
		// question can legitimately offer the User an option named "land".
		if decision.Kind == "merge_ready" {
			for j, option := range decision.Options {
				if option == "land" {
					decision.Options[j] = "merge"
				}
			}
			if decision.Answer == "land" {
				decision.Answer = "merge"
			}
		}
	}
	for i := range state.ResourceLeases {
		lease := &state.ResourceLeases[i]
		if strings.HasPrefix(lease.SourceKey, "mount:") {
			lease.SourceKey = "sandbox:" + strings.TrimPrefix(lease.SourceKey, "mount:")
		}
		if lease.SandboxID != 0 {
			lease.Owner = strings.Replace(lease.Owner, " mount-", " sandbox-", 1)
		}
	}
}
