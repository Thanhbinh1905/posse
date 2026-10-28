package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// down stops the Project's Lead and Watch. The Project stays registered,
// but nothing restarts it until the User runs `posse up` in its folder.
// --plan previews a full shutdown; --execute tears every Task down first and
// may run from the live Lead, whose tab it closes last.
func (s *Service) down(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("down", args, map[string]flagSpec{"plan": {boolean: true}, "execute": {boolean: true}, "user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("down takes no arguments; run it in the Project folder")
	}
	execute, quote := parsed.Bool("execute"), parsed.Flags["user-approved"]
	if parsed.Bool("plan") {
		if execute || quote != "" {
			return axi.Usage("down --plan takes no other flags")
		}
		return s.downPlan(ctx)
	}
	if !execute && quote != "" {
		return axi.Usage("--user-approved requires --execute")
	}
	if execute && strings.TrimSpace(quote) == "" {
		return axi.Failure("down_approval_required", "posse down --execute discards unmerged work and needs User approval", false, "Show `posse down --plan` to the User, then pass `--user-approved \"<User's words>\"`")
	}
	dir, err := currentDir()
	if err != nil {
		return err
	}
	db, home, err := s.openDB()
	if err != nil {
		if !storeBusyOpenFailure(err) {
			return err
		}
		readDB, readErr := store.OpenReadOnly(home)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		request, validationErr := s.validateDownRequest(ctx.Context, readDB, execute)
		closeErr := readDB.Close()
		if validationErr != nil {
			return validationErr
		}
		if closeErr != nil {
			return closeErr
		}
		if stopErr := stopProjectPollerAfterContention(ctx.Context, home, request.project); stopErr != nil {
			return errors.Join(err, stopErr)
		}
		// Retry opening the database only after the read-only validation above
		// identifies and stops this Project's poller.
		db, home, err = s.openDBForDown(ctx.Context, dir)
		if err != nil {
			return err
		}
	}
	defer db.Close()
	request, err := s.validateDownRequest(ctx.Context, db, execute)
	if err != nil {
		return err
	}
	project, callerPaneID := request.project, request.callerPaneID
	var tornDown []any
	if execute {
		if tornDown, err = s.teardownTasksForDown(ctx.Context, db, project, quote); err != nil {
			return err
		}
	}
	// If the Watch is wedged while holding SQLite's writer lock, stop that
	// exact Project process and retry the state change once its connection closes.
	// Mark the Project down before closing anything, so the pane and
	// workspace close events this command causes do not recover it.
	if err := db.MarkProjectDown(ctx.Context, project.ID); err != nil {
		if !store.IsBusy(err) {
			return err
		}
		if stopErr := stopProjectPollerAfterContention(ctx.Context, home, project); stopErr != nil {
			return errors.Join(err, stopErr)
		}
		if retryErr := db.MarkProjectDown(ctx.Context, project.ID); retryErr != nil {
			return retryErr
		}
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
	var callerLead *herdr.Pane
	for _, pane := range snapshot.Panes {
		isLead := pane.Label == leadLabel || project.LeadPaneID != "" && pane.PaneID == project.LeadPaneID && pane.WorkspaceID == project.HerdrWorkspaceID
		if !isLead && pane.Label != watchTabLabel(project) {
			continue
		}
		if request.liveLead && pane.PaneID == callerPaneID {
			// This command runs inside the Lead; closing its tab ends it.
			callerLead = &pane
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
		if err := stopStoredWatchRuntimeForDown(ctx.Context, home, watch); err != nil {
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
	// The next `posse up` keeps the Lead's kind but starts a new conversation.
	if recorded, err := db.ProjectLeadAgent(ctx.Context, project.ID); err != nil {
		return err
	} else if recorded.Session != "" {
		if err := db.SetProjectLeadAgent(ctx.Context, project.ID, store.LeadAgent{Kind: recorded.Kind}); err != nil {
			return err
		}
	}
	if err := s.regenerateProjects(ctx.Context, db); err != nil {
		return err
	}
	if callerLead != nil {
		if err := s.closeOwnPane(ctx.Context, snapshot, *callerLead); err != nil {
			return err
		}
		closed = append(closed, callerLead.PaneID)
	}
	result := axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "state", Value: "down"},
		{Key: "closed_panes", Value: stringsToAny(closed)},
	}
	if execute {
		result = append(result, axi.Field{Key: "torn_down", Value: tornDown})
	}
	if len(kept) > 0 {
		result = append(result, axi.Field{Key: "kept_panes", Value: stringsToAny(kept)})
	}
	return ctx.Print(append(result, axi.Field{Key: "help", Value: []any{"Run `posse up` here to start the Lead again"}}))
}

type downRequest struct {
	project      store.Project
	callerPaneID string
	liveLead     bool
}

func (s *Service) downTarget(ctx context.Context, db *store.DB) (store.Project, caller, error) {
	dir, err := currentDir()
	if err != nil {
		return store.Project{}, caller{}, err
	}
	actor := s.configCaller(ctx, db)
	if actor.role == "worker" || actor.role == "agent" {
		return store.Project{}, caller{}, axi.Failure("user_only", "posse down cannot run from a Worker or agent context", false, "Run `posse down` from a User shell in the Project folder")
	}
	if task, taskErr := taskForPath(ctx, db, dir); taskErr == nil {
		plan, planErr := task.AdmittedPlan()
		if planErr != nil {
			return store.Project{}, caller{}, admittedPlanFailure(planErr)
		}
		if plan == nil || plan.Execution.Mode != "in-place" {
			return store.Project{}, caller{}, axi.Failure("user_only", "posse down cannot run inside a Worker Sandbox", false, "Run `posse down` in the Project folder")
		}
	} else if !store.IsNotFound(taskErr) {
		return store.Project{}, caller{}, taskErr
	}
	project, registered, err := registeredProjectFor(ctx, db, dir)
	if err != nil {
		return store.Project{}, caller{}, err
	}
	if !registered {
		return store.Project{}, caller{}, axi.Failure("project_unknown", "this folder is not registered as a Project", false, "Run `posse down` in a registered Project folder; `posse` lists them")
	}
	if actor.paneID == "" {
		actor.paneID = os.Getenv("HERDR_PANE_ID")
	}
	return project, actor, nil
}

func (s *Service) validateDownRequest(ctx context.Context, db *store.DB, execute bool) (downRequest, error) {
	project, actor, err := s.downTarget(ctx, db)
	if err != nil {
		return downRequest{}, err
	}
	request := downRequest{project: project, callerPaneID: actor.paneID}
	if actor.role == "lead" && s.recordedLeadIsLive(ctx, actor.paneID) {
		if !execute {
			return downRequest{}, axi.Failure("user_only", "posse down can only be run by the User", false, "Exit the Lead agent, or run `posse down` from another shell in the Project folder")
		}
		if actor.projectID != project.ID {
			return downRequest{}, axi.Failure("user_only", "a Lead can only stop its own Project", false)
		}
		request.liveLead = true
	}
	tasks, err := db.LiveTasks(ctx, project.ID)
	if err != nil {
		return downRequest{}, err
	}
	var blocking []string
	for _, task := range tasks {
		if !execute || task.State == store.StateMerging {
			blocking = append(blocking, taskIDString(task.Seq)+" ("+string(task.State)+")")
		}
	}
	if len(blocking) > 0 && execute {
		return downRequest{}, axi.Failure("tasks_merging", fmt.Sprintf("Project %s has Tasks merging: %s", project.Name, strings.Join(blocking, ", ")), false, "Wait for the merge to finish, then run `posse down --plan` again")
	}
	if len(blocking) > 0 {
		return downRequest{}, axi.Failure("tasks_open", fmt.Sprintf("Project %s still has running Tasks: %s", project.Name, strings.Join(blocking, ", ")), false, "Finish or teardown them first; `posse list` shows them")
	}
	if s.Herdr == nil {
		return downRequest{}, axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true)
	}
	return request, nil
}

// teardownTasksForDown cancels queued Tasks first, so none is admitted into a
// slot freed below, then tears down the rest. Unmerged work is discarded under
// the User's quote; recorded pull requests and pushed branches stay.
func (s *Service) teardownTasksForDown(ctx context.Context, db *store.DB, project store.Project, quote string) ([]any, error) {
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return nil, err
	}
	var tornDown []any
	for _, task := range tasks {
		if task.State != store.StateQueued {
			continue
		}
		if err := db.Transition(ctx, task.ID, store.StateQueued, store.StateTornDown, "cli", "Canceled by posse down"); err != nil {
			return nil, err
		}
		tornDown = append(tornDown, taskIDString(task.Seq))
	}
	cfg, err := s.prepareProject(ctx, db, project)
	if err != nil {
		return nil, err
	}
	for _, listed := range tasks {
		task, err := db.TaskByID(ctx, project.ID, listed.ID)
		if err != nil {
			return nil, err
		}
		if task.State == store.StateTornDown || listed.State == store.StateQueued {
			continue
		}
		if task.State == store.StateMerging {
			return nil, axi.Failure("tasks_merging", "Task "+taskIDString(task.Seq)+" started merging during posse down", false, "Wait for the merge to finish, then run `posse down --plan` again")
		}
		plan, planErr := task.AdmittedPlan()
		if planErr != nil {
			return nil, admittedPlanFailure(planErr)
		}
		inPlace := plan != nil && plan.Execution.Mode == "in-place"
		discard := !inPlace && task.State != store.StateMerged && task.State != store.StateReported
		taskQuote := ""
		if discard || inPlace && (task.State == store.StateFailed || task.State == store.StateLost) {
			taskQuote = quote
		}
		if _, err := s.teardownTask(ctx, db, project, cfg, task, discard, taskQuote, teardownOptions{keepPullRequests: true}); err != nil {
			return nil, err
		}
		tornDown = append(tornDown, taskIDString(task.Seq))
	}
	return tornDown, nil
}

// downPlan prints what `posse down --execute` would do and changes nothing.
func (s *Service) downPlan(ctx *axi.Context) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	db, err := store.OpenReadOnly(home)
	if err != nil {
		return err
	}
	defer db.Close()
	project, _, err := s.downTarget(ctx.Context, db)
	if err != nil {
		return err
	}
	tasks, err := db.Tasks(ctx.Context, project.ID, true)
	if err != nil {
		return err
	}
	taskRows, lost, kept, merging := []any{}, []any{}, []any{}, []string{}
	for _, task := range tasks {
		if task.State == store.StateTornDown {
			continue
		}
		id := taskIDString(task.Seq)
		taskRows = append(taskRows, axi.Object{{Key: "task", Value: id}, {Key: "state", Value: string(task.State)}, {Key: "title", Value: task.Title}})
		if task.State == store.StateMerging {
			merging = append(merging, id)
		}
		if task.PRURL != "" {
			kept = append(kept, axi.Object{{Key: "task", Value: id}, {Key: "branch", Value: task.Branch}, {Key: "pr", Value: task.PRURL}})
		}
		plan, planErr := task.AdmittedPlan()
		if planErr != nil {
			return admittedPlanFailure(planErr)
		}
		if task.State == store.StateQueued || task.State == store.StateMerged || task.State == store.StateReported || plan != nil && plan.Execution.Mode == "in-place" {
			continue
		}
		work, err := s.unmergedWork(ctx.Context, db, project, task)
		if err != nil {
			return err
		}
		if work != "" {
			lost = append(lost, axi.Object{{Key: "task", Value: id}, {Key: "work", Value: work}})
		}
	}
	decisions, err := db.PendingDecisions(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	decisionRows := []any{}
	for _, decision := range decisions {
		decisionRows = append(decisionRows, axi.Object{{Key: "id", Value: decision.ID}, {Key: "question", Value: decision.Question}})
	}
	notices, err := db.Notices(ctx.Context, project.ID, true)
	if err != nil {
		return err
	}
	noticeRows := []any{}
	for _, notice := range notices {
		noticeRows = append(noticeRows, axi.Object{{Key: "id", Value: notice.ID}, {Key: "kind", Value: notice.Kind}, {Key: "summary", Value: notice.Summary}})
	}
	help := "After the User approves this plan, run `posse down --execute --user-approved \"<User's words>\"`"
	if len(merging) > 0 {
		help = "posse down --execute refuses while " + strings.Join(merging, ", ") + " is merging; wait for the merge to finish"
	}
	return ctx.Print(axi.Object{
		{Key: "project", Value: project.Name},
		{Key: "tasks", Value: taskRows},
		{Key: "work_lost", Value: lost},
		{Key: "kept_prs", Value: kept},
		{Key: "kept_decisions", Value: decisionRows},
		{Key: "kept_notices", Value: noticeRows},
		{Key: "help", Value: []any{help}},
	})
}

func storeBusyOpenFailure(err error) bool {
	if store.IsBusy(err) {
		return true
	}
	var failure *axi.Error
	return errors.As(err, &failure) && failure.Code == "store_busy"
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

func (s *Service) openDBForDown(ctx context.Context, dir string) (*store.DB, string, error) {
	db, home, err := s.openDB()
	if err == nil || !retryableStoreBusy(err) {
		return db, home, err
	}
	if s.canStopProjectPollerFromDown(ctx) {
		if _, stopErr := stopProjectPollerAtDirectoryAfterContention(ctx, home, dir); stopErr != nil {
			return nil, home, errors.Join(err, stopErr)
		}
	}
	for retry := 0; retry < 2; retry++ {
		timer := time.NewTimer(time.Duration(50<<retry) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, home, ctx.Err()
		case <-timer.C:
		}
		db, home, err = s.openDB()
		if err == nil || !retryableStoreBusy(err) {
			return db, home, err
		}
	}
	return nil, home, err
}

func retryableStoreBusy(err error) bool {
	var failure *axi.Error
	return errors.As(err, &failure) && failure.Code == "store_busy" && failure.Retryable
}

func (s *Service) canStopProjectPollerFromDown(ctx context.Context) bool {
	if os.Getenv(workerHomeEnv) != "" {
		return false
	}
	paneID := os.Getenv("HERDR_PANE_ID")
	for _, ancestor := range processAncestors(procRoot, os.Getpid()) {
		if ancestor.env[workerHomeEnv] != "" || ancestor.agent {
			return false
		}
		for _, key := range agentEnvironment {
			if ancestor.env[key] != "" {
				return false
			}
		}
		if paneID == "" {
			paneID = ancestor.env["HERDR_PANE_ID"]
		}
	}
	for _, key := range agentEnvironment {
		if os.Getenv(key) != "" {
			return false
		}
	}
	if paneID == "" {
		return true
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return false
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID {
			return pane.Agent == "" || pane.AgentStatus == "exited" || pane.AgentStatus == "stopped"
		}
	}
	return false
}
