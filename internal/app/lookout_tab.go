package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func lookoutTabLabel(project store.Project) string { return "posse:" + project.Name + ":lookout" }

// The Lookout owns only a shell tab in the Lead's workspace. It never claims
// Notices or types into the Lead; the configured delivery integration does that.
func (s *Service) ensureLookoutTab(ctx context.Context, project store.Project, snapshot herdr.Snapshot) error {
	label := lookoutTabLabel(project)
	home, err := s.homePath()
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	command := "POSSE_HOME=" + shellQuote(home) + " " + shellQuote(binary) + " lookout --poll-only"
	for _, pane := range snapshot.Panes {
		if pane.Label != label {
			continue
		}
		if pane.WorkspaceID == project.HerdrWorkspaceID {
			if lookoutProcessRunning(pane.PaneID, home) {
				return nil
			}
			_, err := s.herdrCall(ctx, "pane.send_input", map[string]any{"pane_id": pane.PaneID, "text": command, "keys": []string{"enter"}})
			return err
		}
		// A replacement Lead may use another workspace. Close only the old
		// dedicated Lookout tab, never a tab containing foreign panes.
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
	}
	raw, err := s.herdrCall(ctx, "tab.create", map[string]any{"workspace_id": project.HerdrWorkspaceID, "cwd": project.Root, "label": label, "focus": false})
	if err != nil {
		return err
	}
	var opened struct {
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
			TabID  string `json:"tab_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &opened); err != nil {
		return err
	}
	if opened.RootPane.PaneID == "" {
		return fmt.Errorf("lookout tab has no root pane")
	}
	tabID := opened.RootPane.TabID
	if tabID == "" {
		tabID = opened.Tab.TabID
	}
	fail := func(err error) error {
		if tabID != "" {
			_, _ = s.herdrCall(ctx, "tab.close", map[string]any{"tab_id": tabID})
		}
		return err
	}
	if _, err := s.herdrCall(ctx, "pane.rename", map[string]any{"pane_id": opened.RootPane.PaneID, "label": label}); err != nil {
		return fail(err)
	}
	if _, err := s.herdrCall(ctx, "pane.send_input", map[string]any{"pane_id": opened.RootPane.PaneID, "text": command, "keys": []string{"enter"}}); err != nil {
		return fail(err)
	}
	return nil
}

func (s *Service) watchPullRequestsInLookoutTab(ctx *axi.Context, db *store.DB, project store.Project, timeout time.Duration) error {
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return err
	}
	found := false
	for _, pane := range snapshot.Panes {
		if pane.PaneID == os.Getenv("HERDR_PANE_ID") && pane.WorkspaceID == project.HerdrWorkspaceID && pane.Label == lookoutTabLabel(project) {
			found = true
			break
		}
	}
	if !found {
		return axi.Failure("lookout_only", "poll-only mode requires this Project's Lookout tab", false)
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	lastFailure := ""
	for {
		fresh, err := db.ProjectByID(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		if fresh.HerdrWorkspaceID != project.HerdrWorkspaceID {
			return nil
		}
		if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
			if lastFailure != err.Error() {
				_, _ = db.CreateNotice(ctx.Context, store.Notice{ProjectID: project.ID, Kind: "pr_watch_failing", Summary: "Lookout reconcile failed: " + truncate(err.Error(), 240), DataJSON: `{}`})
				lastFailure = err.Error()
			}
		} else {
			lastFailure = ""
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
