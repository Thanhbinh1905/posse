package app

import (
	"context"
	"fmt"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func watchTabLabel(project store.Project) string { return "posse:" + project.Name + ":watch" }

// lastFailures is keyed by phase: one persistent cause usually fails both
// phases, and alternating summaries must not raise a Notice every pass.
func recordWatchFailure(ctx *axi.Context, db *store.DB, projectID int64, lastFailures map[string]string, phase string, err error) {
	if store.IsOnlyBusy(err) {
		summary := "Watch " + phase + " deferred due to transient store contention; will retry"
		if summary != lastFailures[phase] {
			fmt.Fprintln(ctx.ErrOut, summary)
			lastFailures[phase] = summary
		}
		return
	}
	visible := store.WithoutBusy(err)
	if visible == nil {
		visible = err
	}
	summary := "Watch " + phase + " failed: " + truncate(visible.Error(), 240)
	if summary != lastFailures[phase] {
		_, _ = db.CreateNotice(ctx.Context, store.Notice{ProjectID: projectID, Kind: "pr_watch_failing", Summary: summary, DataJSON: `{}`})
		lastFailures[phase] = summary
	}
}

// closeWatchPane closes only the legacy Watch tab, never foreign panes in
// a shared tab. Existing tabs are removed during `posse up` or recovery.
func (s *Service) closeWatchPane(ctx context.Context, pane herdr.Pane, snapshot herdr.Snapshot) error {
	whole := pane.TabID != ""
	for _, other := range snapshot.Panes {
		if other.TabID == pane.TabID && other.PaneID != pane.PaneID {
			whole = false
		}
	}
	method, params := "pane.close", map[string]any{"pane_id": pane.PaneID}
	if whole {
		method, params = "tab.close", map[string]any{"tab_id": pane.TabID}
	}
	if _, err := s.herdrCall(ctx, method, params); err != nil && !missingPaneError(err) {
		return err
	}
	return nil
}

func (s *Service) cleanupLegacyWatchTabs(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot) error {
	state, err := db.WatchRecovery(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, pane := range snapshot.Panes {
		// A historical label is stored custody evidence, not an input alias.
		recordedLegacy := pane.PaneID == state.PaneID && pane.Label == "posse:"+project.Name+":lookout"
		if pane.WorkspaceID != project.HerdrWorkspaceID || pane.Label != watchTabLabel(project) && !recordedLegacy {
			continue
		}
		if pane.Agent != "" {
			return axi.Failure("watch_upgrade_blocked", "the former Watch pane now contains an agent; leave it intact", false, "Move the agent or clear the old Watch label from a User shell, then retry")
		}
		if err := s.closeWatchPane(ctx, pane, snapshot); err != nil {
			return err
		}
		if pane.PaneID == state.PaneID {
			state.PaneID = ""
			if err := db.SetWatchRecovery(ctx, project.ID, state); err != nil {
				return err
			}
		}
	}
	return nil
}
