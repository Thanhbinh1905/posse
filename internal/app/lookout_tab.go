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

var lookoutStartupGracePeriod = 5 * time.Second
var lookoutRetryBasePeriod = time.Second
var lookoutRetryMaxPeriod = time.Minute

const lookoutStartFailureNoticeAfter = 3

func delayLookoutReplacement(ctx context.Context, db *store.DB, projectID int64, paneID string, now time.Time) (bool, error) {
	state, err := db.LookoutRecovery(ctx, projectID)
	if err != nil {
		return false, err
	}
	if state.PaneID != paneID || state.RetryAt == 0 {
		state = store.LookoutRecovery{PaneID: paneID, RetryAt: now.Add(lookoutStartupGracePeriod).UnixMilli()}
		return true, db.SetLookoutRecovery(ctx, projectID, state)
	}
	return now.UnixMilli() < state.RetryAt, nil
}

func lookoutRecoveryDue(ctx context.Context, db *store.DB, projectID int64, paneID string, now time.Time) (bool, error) {
	state, err := db.LookoutRecovery(ctx, projectID)
	if err != nil {
		return false, err
	}
	return state.PaneID != paneID || state.RetryAt == 0 || now.UnixMilli() >= state.RetryAt, nil
}

func resetLookoutRecovery(ctx context.Context, db *store.DB, projectID int64) error {
	return db.ResetLookoutRecovery(ctx, projectID)
}

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

func markLookoutRunning(ctx context.Context, db *store.DB, projectID int64, paneID string) error {
	state, err := db.LookoutRecovery(ctx, projectID)
	if err != nil {
		return err
	}
	if state.PaneID == paneID && state.RetryAt == 0 && state.FailedStarts == 0 && !state.NoticeRaised {
		return nil
	}
	state.PaneID = paneID
	state.RetryAt = 0
	state.FailedStarts = 0
	state.NoticeRaised = false
	return db.SetLookoutRecovery(ctx, projectID, state)
}

func markLookoutCreated(ctx context.Context, db *store.DB, projectID int64, paneID string, failedStart bool) error {
	state := store.LookoutRecovery{PaneID: paneID}
	if failedStart {
		previous, err := db.LookoutRecovery(ctx, projectID)
		if err != nil {
			return err
		}
		state.FailedStarts = previous.FailedStarts + 1
		state.NoticeRaised = previous.NoticeRaised
	}
	delay := lookoutStartupGracePeriod
	if failedStart {
		delay += lookoutRetryBackoff(state.FailedStarts)
	}
	state.RetryAt = time.Now().Add(delay).UnixMilli()
	return db.SetLookoutRecovery(ctx, projectID, state)
}

func (s *Service) waitLookoutRecovery(ctx context.Context, project store.Project, paneID, home string) (bool, error) {
	timer := time.NewTimer(lookoutStartupGracePeriod)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	defer ticker.Stop()
	for {
		if lookoutProcessRunning(paneID, home) {
			return true, nil
		}
		if snapshot, err := s.snapshot(ctx); err == nil {
			present := false
			for _, pane := range snapshot.Panes {
				if pane.PaneID == paneID && pane.WorkspaceID == project.HerdrWorkspaceID && pane.Label == lookoutTabLabel(project) {
					present = true
					break
				}
			}
			if !present {
				return false, nil
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return lookoutProcessRunning(paneID, home), nil
		case <-ticker.C:
		}
	}
}

func lookoutRetryBackoff(failedStarts int) time.Duration {
	if failedStarts < 1 {
		return 0
	}
	delay := lookoutRetryBasePeriod
	for i := 1; i < failedStarts && delay < lookoutRetryMaxPeriod; i++ {
		delay *= 2
	}
	if delay > lookoutRetryMaxPeriod {
		return lookoutRetryMaxPeriod
	}
	return delay
}

func lookoutStartFailureNotice(ctx context.Context, db *store.DB, projectID int64) (int, bool, error) {
	state, err := db.LookoutRecovery(ctx, projectID)
	return state.FailedStarts, state.NoticeRaised, err
}

func markLookoutStartFailureNotice(ctx context.Context, db *store.DB, projectID int64) error {
	state, err := db.LookoutRecovery(ctx, projectID)
	if err != nil {
		return err
	}
	state.NoticeRaised = true
	return db.SetLookoutRecovery(ctx, projectID, state)
}

// The Lookout owns only a shell tab in the Lead's workspace. It never claims
// Notices or types into the Lead; the configured delivery integration does that.
func (s *Service) ensureLookoutTab(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot, replaceUnstarted bool) error {
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
	failedStart := false
	for _, pane := range snapshot.Panes {
		if pane.Label != label {
			continue
		}
		if pane.WorkspaceID == project.HerdrWorkspaceID && (pane.PaneID == os.Getenv("HERDR_PANE_ID") || lookoutProcessRunning(pane.PaneID, home)) {
			return markLookoutRunning(ctx, db, project.ID, pane.PaneID)
		}
		if pane.WorkspaceID == project.HerdrWorkspaceID {
			if replaceUnstarted {
				if err := resetLookoutRecovery(ctx, db, project.ID); err != nil {
					return err
				}
			} else {
				delayed, err := delayLookoutReplacement(ctx, db, project.ID, pane.PaneID, time.Now())
				if err != nil {
					return err
				}
				if delayed {
					return nil
				}
				failedStart = true
			}
		}
		// A label is not proof that the shell is idle. Close its dedicated
		// tab before starting a replacement; never type into a live pane.
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
	if !failedStart {
		if err := resetLookoutRecovery(ctx, db, project.ID); err != nil {
			return err
		}
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
	return markLookoutCreated(ctx, db, project.ID, opened.RootPane.PaneID, failedStart)
}

func (s *Service) watchPullRequestsInLookoutTab(ctx *axi.Context, db *store.DB, project store.Project, timeout time.Duration, stopSignals <-chan os.Signal) error {
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
		select {
		case <-stopSignals:
			return s.reportLookoutStopped(ctx, project, true)
		default:
		}
		fresh, err := db.ProjectByID(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		if fresh.HerdrWorkspaceID != project.HerdrWorkspaceID {
			return nil
		}
		if _, err := s.prepareProjectObservation(ctx.Context, db, project); err != nil {
			recordLookoutFailure(ctx, db, project.ID, &lastFailure, "reconcile", err)
		}
		if err := s.maintainProjectWatch(ctx.Context, db, project); err != nil {
			recordLookoutFailure(ctx, db, project.ID, &lastFailure, "watch", err)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-stopSignals:
			return s.reportLookoutStopped(ctx, project, true)
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
