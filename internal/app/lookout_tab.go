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

type lookoutRecoveryState struct {
	paneID       string
	retryAt      time.Time
	failedStarts int
	noticeRaised bool
}

func (s *Service) delayLookoutReplacement(projectID int64, paneID string, now time.Time) bool {
	s.lookoutMu.Lock()
	defer s.lookoutMu.Unlock()
	if s.lookoutRecovery == nil {
		s.lookoutRecovery = make(map[int64]lookoutRecoveryState)
	}
	state, found := s.lookoutRecovery[projectID]
	if !found || state.paneID != paneID {
		state = lookoutRecoveryState{paneID: paneID, retryAt: now.Add(lookoutStartupGracePeriod)}
		s.lookoutRecovery[projectID] = state
		return true
	}
	return now.Before(state.retryAt)
}

func (s *Service) waitLookoutStartup(ctx context.Context, project store.Project, paneID, home string) (running, panePresent bool, err error) {
	for {
		if lookoutProcessRunning(paneID, home) {
			s.markLookoutRunning(project.ID, paneID)
			return true, true, nil
		}
		if snapshot, err := s.snapshot(ctx); err == nil {
			panePresent := false
			for _, pane := range snapshot.Panes {
				if pane.PaneID == paneID && pane.WorkspaceID == project.HerdrWorkspaceID && pane.Label == lookoutTabLabel(project) {
					panePresent = true
					break
				}
			}
			if !panePresent {
				return false, false, nil
			}
		}
		s.lookoutMu.Lock()
		state, found := s.lookoutRecovery[project.ID]
		s.lookoutMu.Unlock()
		if !found || state.paneID != paneID {
			return false, false, nil
		}
		remaining := time.Until(state.retryAt)
		if remaining <= 0 {
			return false, true, nil
		}
		wait := min(100*time.Millisecond, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false, true, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Service) lookoutRecoveryDue(projectID int64, paneID string, now time.Time) bool {
	s.lookoutMu.Lock()
	defer s.lookoutMu.Unlock()
	state, found := s.lookoutRecovery[projectID]
	return !found || state.paneID != paneID || !now.Before(state.retryAt)
}

func (s *Service) resetLookoutRecovery(projectID int64) {
	s.lookoutMu.Lock()
	defer s.lookoutMu.Unlock()
	delete(s.lookoutRecovery, projectID)
}

func (s *Service) markLookoutRunning(projectID int64, paneID string) {
	s.lookoutMu.Lock()
	defer s.lookoutMu.Unlock()
	if s.lookoutRecovery == nil {
		s.lookoutRecovery = make(map[int64]lookoutRecoveryState)
	}
	s.lookoutRecovery[projectID] = lookoutRecoveryState{paneID: paneID}
}

func (s *Service) markLookoutCreated(projectID int64, paneID string, failedStart bool) {
	s.lookoutMu.Lock()
	defer s.lookoutMu.Unlock()
	if s.lookoutRecovery == nil {
		s.lookoutRecovery = make(map[int64]lookoutRecoveryState)
	}
	previous := s.lookoutRecovery[projectID]
	state := lookoutRecoveryState{paneID: paneID}
	if failedStart {
		state.failedStarts = previous.failedStarts + 1
		state.noticeRaised = previous.noticeRaised
	}
	delay := lookoutStartupGracePeriod
	if failedStart {
		delay += lookoutRetryBackoff(state.failedStarts)
	}
	state.retryAt = time.Now().Add(delay)
	s.lookoutRecovery[projectID] = state
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

func (s *Service) lookoutStartFailureNotice(projectID int64) (int, bool) {
	s.lookoutMu.Lock()
	defer s.lookoutMu.Unlock()
	state := s.lookoutRecovery[projectID]
	return state.failedStarts, state.noticeRaised
}

func (s *Service) markLookoutStartFailureNotice(projectID int64) {
	s.lookoutMu.Lock()
	defer s.lookoutMu.Unlock()
	state := s.lookoutRecovery[projectID]
	state.noticeRaised = true
	s.lookoutRecovery[projectID] = state
}

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
	failedStart := false
	for _, pane := range snapshot.Panes {
		if pane.Label != label {
			continue
		}
		if pane.WorkspaceID == project.HerdrWorkspaceID && (pane.PaneID == os.Getenv("HERDR_PANE_ID") || lookoutProcessRunning(pane.PaneID, home)) {
			s.markLookoutRunning(project.ID, pane.PaneID)
			return nil
		}
		if pane.WorkspaceID == project.HerdrWorkspaceID {
			if s.delayLookoutReplacement(project.ID, pane.PaneID, time.Now()) {
				running, panePresent, err := s.waitLookoutStartup(ctx, project, pane.PaneID, home)
				if err != nil {
					return err
				}
				if running {
					return nil
				}
				if !panePresent {
					s.resetLookoutRecovery(project.ID)
				} else {
					failedStart = true
				}
			} else {
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
		s.resetLookoutRecovery(project.ID)
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
	s.markLookoutCreated(project.ID, opened.RootPane.PaneID, failedStart)
	return nil
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
		if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
			if lastFailure != err.Error() {
				_, _ = db.CreateNotice(ctx.Context, store.Notice{ProjectID: project.ID, Kind: "pr_watch_failing", Summary: "Lookout reconcile failed: " + truncate(err.Error(), 240), DataJSON: `{}`})
				lastFailure = err.Error()
			}
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
