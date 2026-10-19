package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func watchTabLabel(project store.Project) string { return "posse:" + project.Name + ":watch" }

var watchStartupGracePeriod = 5 * time.Second
var watchRetryBasePeriod = time.Second
var watchRetryMaxPeriod = time.Minute

const watchStartFailureNoticeAfter = 3

func delayWatchReplacement(ctx context.Context, db *store.DB, projectID int64, paneID string, now time.Time) (bool, error) {
	state, err := db.WatchRecovery(ctx, projectID)
	if err != nil {
		return false, err
	}
	if state.PaneID != paneID || state.RetryAt == 0 {
		state = store.WatchRecovery{PaneID: paneID, RetryAt: now.Add(watchStartupGracePeriod).UnixMilli()}
		return true, db.SetWatchRecovery(ctx, projectID, state)
	}
	return now.UnixMilli() < state.RetryAt, nil
}

func watchRecoveryDue(ctx context.Context, db *store.DB, projectID int64, paneID string, now time.Time) (bool, error) {
	state, err := db.WatchRecovery(ctx, projectID)
	if err != nil {
		return false, err
	}
	return state.PaneID != paneID || state.RetryAt == 0 || now.UnixMilli() >= state.RetryAt, nil
}

func resetWatchRecovery(ctx context.Context, db *store.DB, projectID int64) error {
	return db.ResetWatchRecovery(ctx, projectID)
}

func recordWatchFailure(ctx *axi.Context, db *store.DB, projectID int64, lastFailure *string, phase string, err error) {
	if store.IsOnlyBusy(err) {
		summary := "Watch " + phase + " deferred due to transient store contention; will retry"
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
	summary := "Watch " + phase + " failed: " + truncate(visible.Error(), 240)
	if summary != *lastFailure {
		_, _ = db.CreateNotice(ctx.Context, store.Notice{ProjectID: projectID, Kind: "pr_watch_failing", Summary: summary, DataJSON: `{}`})
		*lastFailure = summary
	}
}

func markWatchRunning(ctx context.Context, db *store.DB, projectID int64, paneID string) error {
	state, err := db.WatchRecovery(ctx, projectID)
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
	return db.SetWatchRecovery(ctx, projectID, state)
}

func markWatchCreated(ctx context.Context, db *store.DB, projectID int64, paneID string, failedStart bool) error {
	state := store.WatchRecovery{PaneID: paneID}
	if failedStart {
		previous, err := db.WatchRecovery(ctx, projectID)
		if err != nil {
			return err
		}
		state.FailedStarts = previous.FailedStarts + 1
		state.NoticeRaised = previous.NoticeRaised
	}
	delay := watchStartupGracePeriod
	if failedStart {
		delay += watchRetryBackoff(state.FailedStarts)
	}
	state.RetryAt = time.Now().Add(delay).UnixMilli()
	return db.SetWatchRecovery(ctx, projectID, state)
}

func (s *Service) waitWatchRecovery(ctx context.Context, project store.Project, paneID, home string) (bool, error) {
	timer := time.NewTimer(watchStartupGracePeriod)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	defer ticker.Stop()
	for {
		if watchProcessRunning(paneID, home) {
			return true, nil
		}
		if snapshot, err := s.snapshot(ctx); err == nil {
			present := false
			for _, pane := range snapshot.Panes {
				if pane.PaneID == paneID && pane.WorkspaceID == project.HerdrWorkspaceID && pane.Label == watchTabLabel(project) {
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
			return watchProcessRunning(paneID, home), nil
		case <-ticker.C:
		}
	}
}

func watchRetryBackoff(failedStarts int) time.Duration {
	if failedStarts < 1 {
		return 0
	}
	delay := watchRetryBasePeriod
	for i := 1; i < failedStarts && delay < watchRetryMaxPeriod; i++ {
		delay *= 2
	}
	if delay > watchRetryMaxPeriod {
		return watchRetryMaxPeriod
	}
	return delay
}

func watchStartFailureNotice(ctx context.Context, db *store.DB, projectID int64) (int, bool, error) {
	state, err := db.WatchRecovery(ctx, projectID)
	return state.FailedStarts, state.NoticeRaised, err
}

func markWatchStartFailureNotice(ctx context.Context, db *store.DB, projectID int64) error {
	state, err := db.WatchRecovery(ctx, projectID)
	if err != nil {
		return err
	}
	state.NoticeRaised = true
	return db.SetWatchRecovery(ctx, projectID, state)
}

// The Watch owns only a shell tab in the Lead's workspace. It never claims
// Notices or types into the Lead; the configured delivery integration does that.
func (s *Service) ensureWatchTab(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot, replaceUnstarted bool) error {
	label := watchTabLabel(project)
	home, err := s.homePath()
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	command := "POSSE_HOME=" + shellQuote(home) + " " + shellQuote(binary) + " watch --poll-only"
	state, err := db.WatchRecovery(ctx, project.ID)
	if err != nil {
		return err
	}
	// An upgrade replaces the recorded pre-rename observer, not an arbitrary
	// labelled pane. Its old binary cannot observe the migrated schema.
	legacyLabel := "posse:" + project.Name + ":lookout"
	for _, pane := range snapshot.Panes {
		if pane.PaneID == state.PaneID && pane.Label == legacyLabel && pane.WorkspaceID == project.HerdrWorkspaceID {
			if pane.Agent != "" {
				return axi.Failure("watch_upgrade_blocked", "the previous observer pane now contains an agent; preserve it and inspect its ownership", false)
			}
			if err := s.closeWatchPane(ctx, pane, snapshot); err != nil {
				return err
			}
		}
	}
	var runningPane *herdr.Pane
	for index := range snapshot.Panes {
		pane := &snapshot.Panes[index]
		if pane.Label != label || pane.WorkspaceID != project.HerdrWorkspaceID {
			continue
		}
		if pane.PaneID != os.Getenv("HERDR_PANE_ID") && !watchProcessRunning(pane.PaneID, home) {
			continue
		}
		if runningPane == nil || pane.PaneID == state.PaneID {
			runningPane = pane
		}
	}
	if runningPane != nil {
		for _, pane := range snapshot.Panes {
			if pane.Label == label && pane.PaneID != runningPane.PaneID {
				if err := s.closeWatchPane(ctx, pane, snapshot); err != nil {
					return err
				}
			}
		}
		return markWatchRunning(ctx, db, project.ID, runningPane.PaneID)
	}

	failedStart := false
	resetRecovery := false
	for _, pane := range snapshot.Panes {
		if pane.Label != label {
			continue
		}
		if pane.WorkspaceID == project.HerdrWorkspaceID {
			if replaceUnstarted {
				if !resetRecovery {
					if err := resetWatchRecovery(ctx, db, project.ID); err != nil {
						return err
					}
					resetRecovery = true
				}
			} else {
				delayed, err := delayWatchReplacement(ctx, db, project.ID, pane.PaneID, time.Now())
				if err != nil {
					return err
				}
				if delayed {
					return nil
				}
				failedStart = true
			}
		}
		if err := s.closeWatchPane(ctx, pane, snapshot); err != nil {
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
		return fmt.Errorf("watch tab has no root pane")
	}
	if !failedStart {
		if err := resetWatchRecovery(ctx, db, project.ID); err != nil {
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
	return markWatchCreated(ctx, db, project.ID, opened.RootPane.PaneID, failedStart)
}

// closeWatchPane closes only the dedicated Watch tab, never foreign panes in a shared tab.
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

func (s *Service) watchPullRequestsInWatchTab(ctx *axi.Context, db *store.DB, project store.Project, timeout time.Duration, stopSignals <-chan os.Signal) error {
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return err
	}
	found := false
	for _, pane := range snapshot.Panes {
		if pane.PaneID == os.Getenv("HERDR_PANE_ID") && pane.WorkspaceID == project.HerdrWorkspaceID && pane.Label == watchTabLabel(project) {
			found = true
			break
		}
	}
	if !found {
		return axi.Failure("watch_only", "poll-only mode requires this Project's Watch tab", false)
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	lastFailure := ""
	for {
		select {
		case <-stopSignals:
			return s.reportWatchStopped(ctx, project, true)
		default:
		}
		fresh, err := db.ProjectByID(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		if fresh.HerdrWorkspaceID != project.HerdrWorkspaceID {
			return nil
		}
		if _, err := s.prepareProjectObservation(ctx.Context, db, project); err != nil && !errors.Is(err, errRecoveryHeld) && !errors.Is(err, errRecoveryDeferred) {
			recordWatchFailure(ctx, db, project.ID, &lastFailure, "reconcile", err)
		}
		if err := s.maintainProjectWatch(ctx.Context, db, project); err != nil {
			recordWatchFailure(ctx, db, project.ID, &lastFailure, "watch", err)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-stopSignals:
			return s.reportWatchStopped(ctx, project, true)
		case <-ctx.Context.Done():
			return ctx.Context.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
