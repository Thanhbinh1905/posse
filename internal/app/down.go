package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// down stops the Project's Lead and Watch. The Project stays registered,
// but nothing restarts it until the User runs `posse up` in its folder.
func (s *Service) down(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("down", args, map[string]flagSpec{})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("down takes no arguments; run it in the Project folder")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	dir, err := currentDir()
	if err != nil {
		return err
	}
	actor := s.configCaller(ctx.Context, db)
	if actor.role == "worker" || actor.role == "agent" {
		return axi.Failure("user_only", "posse down cannot run from a Worker or agent context", false, "Run `posse down` from a User shell in the Project folder")
	}
	if task, taskErr := taskForPath(ctx.Context, db, dir); taskErr == nil {
		plan, planErr := task.AdmittedPlan()
		if planErr != nil {
			return admittedPlanFailure(planErr)
		}
		if plan == nil || plan.Execution.Mode != "in-place" {
			return axi.Failure("user_only", "posse down cannot run inside a Worker Sandbox", false, "Run `posse down` in the Project folder")
		}
	} else if !store.IsNotFound(taskErr) {
		return taskErr
	}
	project, registered, err := registeredProjectFor(ctx.Context, db, dir)
	if err != nil {
		return err
	}
	if !registered {
		return axi.Failure("project_unknown", "this folder is not registered as a Project", false, "Run `posse down` in a registered Project folder; `posse` lists them")
	}
	// A Lead pane whose agent has exited is the User's shell again.
	callerPaneID := actor.paneID
	if callerPaneID == "" {
		callerPaneID = os.Getenv("HERDR_PANE_ID")
	}
	switch actor.role {
	case "worker", "agent":
		return axi.Failure("user_only", "posse down can only be run by the User", false)
	case "lead":
		if s.recordedLeadIsLive(ctx.Context, callerPaneID) {
			return axi.Failure("user_only", "posse down can only be run by the User", false, "Exit the Lead agent, or run `posse down` from another shell in the Project folder")
		}
	}
	tasks, err := db.LiveTasks(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	if len(tasks) > 0 {
		names := make([]string, 0, len(tasks))
		for _, task := range tasks {
			names = append(names, taskIDString(task.Seq)+" ("+string(task.State)+")")
		}
		return axi.Failure("tasks_open", fmt.Sprintf("Project %s still has running Tasks: %s", project.Name, strings.Join(names, ", ")), false, "Finish or teardown them first; `posse list` shows them")
	}
	if s.Herdr == nil {
		return axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true)
	}
	// Mark the Project down before closing anything, so the pane and
	// workspace close events this command causes do not recover it.
	if err := db.MarkProjectDown(ctx.Context, project.ID); err != nil {
		return err
	}
	snapshot, err := s.snapshot(ctx.Context)
	if err != nil {
		return herdrError(err)
	}
	leadLabel := project.LeadLabel
	if leadLabel == "" {
		leadLabel = "posse:" + project.Name + ":lead"
	}
	var closed, kept []string
	for _, pane := range snapshot.Panes {
		isLead := pane.Label == leadLabel || project.LeadPaneID != "" && pane.PaneID == project.LeadPaneID && pane.WorkspaceID == project.HerdrWorkspaceID
		if !isLead && pane.Label != watchTabLabel(project) {
			continue
		}
		if isLead && pane.Agent != "" {
			// Let the agent save its session; closing its tab below ends one
			// that does not exit.
			_ = s.stopLeadAgent(ctx.Context, pane.PaneID, pane.Agent)
		}
		if pane.PaneID == callerPaneID {
			// Keep the caller's shell; it is no longer the Lead.
			if _, err := s.herdrCall(ctx.Context, "pane.rename", map[string]any{"pane_id": pane.PaneID, "label": ""}); err != nil && !missingPaneError(err) {
				return err
			}
			if _, err := s.herdrCall(ctx.Context, "pane.report_metadata", map[string]any{"pane_id": pane.PaneID, "source": "posse", "clear_title": true, "clear_display_agent": true}); err != nil && !missingPaneError(err) {
				return err
			}
			kept = append(kept, pane.PaneID)
			continue
		}
		if err := s.closeOwnPane(ctx.Context, snapshot, pane); err != nil {
			return err
		}
		closed = append(closed, pane.PaneID)
	}
	processes, err := s.runningWatches(ctx.Context, home)
	if err != nil {
		return err
	}
	var owned, watches, leadWatches []watchProcess
	for _, process := range processes {
		if !processBelongsToProject(process, project) {
			continue
		}
		owned = append(owned, process)
		if process.PollOnly {
			watches = append(watches, process)
		} else {
			leadWatches = append(leadWatches, process)
		}
	}
	if remaining, err := stopWatchProcesses(ctx.Context, home, leadWatches); err != nil {
		return err
	} else if !allWatchesStopped(remaining) {
		return axi.Failure("watch_stop_failed", "a Watch of Project "+project.Name+" is still running", true, "Retry `posse down`")
	}
	for _, watch := range watches {
		if err := stopStoredWatchRuntime(ctx.Context, home, watch); err != nil {
			return axi.Failure("watch_stop_failed", "the Project watch is still completing maintenance", true, "Retry `posse down` after it stops")
		}
	}
	if !allWatchesStopped(owned) {
		return axi.Failure("watch_stop_failed", "a Watch of Project "+project.Name+" is still running", true, "Retry `posse down`")
	}
	watch, err := db.WatchRuntime(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	if watch.PID > 0 {
		if err := db.ClearWatchRuntime(ctx.Context, project.ID, watch.PID, watch.Token); err != nil {
			return err
		}
	}
	if err := db.ClearDownProjectLead(ctx.Context, project.ID); err != nil {
		return err
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	result := axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "state", Value: "down"},
		{Key: "closed_panes", Value: stringsToAny(closed)},
	}
	if len(kept) > 0 {
		result = append(result, axi.Field{Key: "kept_panes", Value: stringsToAny(kept)})
	}
	return ctx.Print(append(result, axi.Field{Key: "help", Value: []any{"Run `posse up` here to start the Lead again"}}))
}

// closeOwnPane closes a Posse pane's tab when the tab holds nothing else, and
// only the pane when the tab also holds panes Posse does not own.
func (s *Service) closeOwnPane(ctx context.Context, snapshot herdr.Snapshot, pane herdr.Pane) error {
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

func stringsToAny(values []string) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
