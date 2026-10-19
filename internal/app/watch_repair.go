package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) repairWatch(ctx *axi.Context, db *store.DB, home string, project store.Project) error {
	actor := s.configCaller(ctx.Context, db)
	switch actor.role {
	case "worker", "agent":
		return axi.Failure("user_only", "only the User can repair a Project Watch", false)
	case "lead":
		if s.recordedLeadIsLive(ctx.Context, actor.paneID) {
			return axi.Failure("user_only", "only the User can repair a Project Watch", false, "Run `posse watch --repair` from the Project folder outside the live Lead pane")
		}
	}
	if cwdInsideSandbox(ctx.Context, db) {
		return axi.Failure("user_only", "Watch repair cannot run from inside a Worker Sandbox", false, "Run `posse watch --repair` from the Project folder")
	}
	if project.IsDown() {
		return axi.Failure("project_down", "the Project is stopped; Watch repair will not restart it", false, "Run `posse up` only when you intend to restart the Lead and recover Workers")
	}
	if _, err := os.Stat(project.Root); err != nil {
		return axi.Failure("project_missing", fmt.Sprintf("Project %s path is unavailable: %s", project.Name, project.Root), false, "Run `posse project move` to update the Project path")
	}
	if s.Herdr == nil {
		return axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true, "Run `posse doctor` to inspect setup")
	}

	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return axi.Failure("watch_repair_failed", "could not verify the existing Lead workspace; Watch was not changed", true, err.Error(), "Restore Herdr, then retry `posse watch --repair`")
	}
	workspaceID, found := leadWorkspace(snapshot, project)
	if !found || workspaceID != project.HerdrWorkspaceID {
		return axi.Failure("lead_workspace_missing", "the recorded Lead workspace is not open; Watch repair will not restart the Lead", false, "Restore the Lead workspace, then retry `posse watch --repair`")
	}

	beforeState, err := db.WatchRecovery(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	beforeProcesses, processErr := systemWatchProcesses(home)
	beforeCheck := projectWatchCheck(project, beforeState, snapshot, beforeProcesses, processErr)

	if err := s.ensureWatchTab(ctx.Context, db, project, snapshot, true); err != nil {
		return axi.Failure("watch_repair_failed", "could not restore the Project's poll-only Watch", true, err.Error(), "Check Herdr availability and retry `posse watch --repair`")
	}
	state, check, err := s.waitWatchRepairVerification(ctx.Context, db, home, project)
	if err != nil {
		return axi.Failure("watch_repair_failed", "could not verify the restored poll-only Watch", true, err.Error(), "Check Herdr availability and retry `posse watch --repair`")
	}
	if check.Status != "ok" {
		return axi.Failure("watch_repair_failed", "Watch repair could not verify one matching pane and process", true, check.Detail, "Inspect Herdr and retry `posse watch --repair`")
	}
	return ctx.Print(axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "pane", Value: state.PaneID},
		{Key: "state", Value: "running"},
		{Key: "repaired", Value: beforeCheck.Status != "ok"},
		{Key: "help", Value: []any{"This operation restores only the poll-only Watch; it does not recover the Lead or Workers or change config"}},
	})
}

func (s *Service) waitWatchRepairVerification(ctx context.Context, db *store.DB, home string, project store.Project) (store.WatchRecovery, doctorCheck, error) {
	deadline := time.Now().Add(watchStartupGracePeriod)
	var state store.WatchRecovery
	var check doctorCheck
	for {
		var err error
		state, err = db.WatchRecovery(ctx, project.ID)
		if err != nil {
			return state, doctorCheck{}, err
		}
		snapshot, err := s.snapshot(ctx)
		if err != nil {
			return state, doctorCheck{}, err
		}
		processes, processErr := systemWatchProcesses(home)
		check = projectWatchCheck(project, state, snapshot, processes, processErr)
		if check.Status == "ok" || !time.Now().Before(deadline) {
			return state, check, nil
		}
		select {
		case <-ctx.Done():
			return state, check, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func projectWatchCheck(project store.Project, state store.WatchRecovery, snapshot herdr.Snapshot, processes []watchProcess, processErr error) doctorCheck {
	panes := make([]herdr.Pane, 0, 1)
	for _, pane := range snapshot.Panes {
		if pane.Label == watchTabLabel(project) {
			panes = append(panes, pane)
		}
	}
	correctPanes := make([]herdr.Pane, 0, len(panes))
	for _, pane := range panes {
		if pane.WorkspaceID == project.HerdrWorkspaceID {
			correctPanes = append(correctPanes, pane)
		}
	}
	pollerPaneIDs := make([]string, 0, 1)
	for _, process := range processes {
		if process.PollOnly && processBelongsToProject(process, project) && watchPIDRunning(process.PID) {
			pollerPaneIDs = append(pollerPaneIDs, process.PaneID)
		}
	}
	leadRunning := liveLeadWatch(project, processes)

	detail := fmt.Sprintf("recorded pane=%s; ", watchPaneValue(state.PaneID))
	switch {
	case len(panes) == 0:
		detail += "poll-only pane missing"
	case len(correctPanes) == 0:
		detail += fmt.Sprintf("poll-only pane mismatched (workspace=%s)", panes[0].WorkspaceID)
	case len(panes) > 1:
		detail += fmt.Sprintf("multiple poll-only panes=%d", len(panes))
	case state.PaneID != correctPanes[0].PaneID:
		detail += fmt.Sprintf("recorded pane mismatch (observed=%s)", correctPanes[0].PaneID)
	default:
		detail += fmt.Sprintf("poll-only pane=%s", correctPanes[0].PaneID)
	}
	if processErr != nil {
		detail += "; poll-only processes=unknown (" + processErr.Error() + ")"
	} else {
		detail += fmt.Sprintf("; poll-only processes=%d", len(pollerPaneIDs))
		if len(pollerPaneIDs) == 1 {
			detail += "; poll-only process pane=" + pollerPaneIDs[0]
		} else if len(pollerPaneIDs) > 1 {
			detail += "; poll-only process panes=" + strings.Join(pollerPaneIDs, ",")
		}
	}
	if processErr != nil {
		detail += "; Lead delivery Watch=unknown"
	} else if leadRunning {
		detail += "; Lead delivery Watch=running"
	} else {
		detail += "; Lead delivery Watch=missing"
	}

	pollerMatchesPane := len(correctPanes) == 1 && len(pollerPaneIDs) == 1 && pollerPaneIDs[0] == correctPanes[0].PaneID
	healthy := len(panes) == 1 && len(correctPanes) == 1 && state.PaneID == correctPanes[0].PaneID && pollerMatchesPane && processErr == nil
	status, action := "ok", ""
	if !healthy {
		status = "warn"
		action = "Run `posse watch --repair` from the Project folder to restore only its poll-only Watch"
	}
	return doctorCheck{Name: "Project Watch " + project.Name, Status: status, Detail: detail, Action: action}
}

func watchPaneValue(paneID string) string {
	if paneID == "" {
		return "none"
	}
	return paneID
}
