package app

import (
	"context"
	"fmt"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func lookoutTabLabel(project store.Project) string { return "posse:" + project.Name + ":lookout" }

func recordLookoutFailure(ctx *axi.Context, db *store.DB, projectID int64, lastFailure *string, phase string, err error) {
	if store.IsOnlyBusy(err) {
		summary := "Lookout " + phase + " deferred due to transient store contention; will retry"
		if summary != *lastFailure {
			fmt.Fprintln(ctx.ErrOut, summary)
			*lastFailure = summary
		}
		return
	}
	visible := store.WithoutBusy(err)
	if visible == nil {
		visible = err
	}
	summary := "Lookout " + phase + " failed: " + truncate(visible.Error(), 240)
	if summary != *lastFailure {
		_, _ = db.CreateNotice(ctx.Context, store.Notice{ProjectID: projectID, Kind: "pr_watch_failing", Summary: summary, DataJSON: `{}`})
		*lastFailure = summary
	}
}

// closeLookoutPane closes only the legacy Lookout tab, never foreign panes in
// a shared tab. Existing tabs are removed during `posse up` or recovery.
func (s *Service) closeLookoutPane(ctx context.Context, pane herdr.Pane, snapshot herdr.Snapshot) error {
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

func (s *Service) cleanupLegacyLookoutTabs(ctx context.Context, project store.Project, snapshot herdr.Snapshot) error {
	for _, pane := range snapshot.Panes {
		if pane.Label != lookoutTabLabel(project) {
			continue
		}
		if err := s.closeLookoutPane(ctx, pane, snapshot); err != nil {
			return err
		}
	}
	return nil
}
