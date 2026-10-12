package app

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

type projectSummary struct {
	Name          string              `json:"name"`
	Mode          string              `json:"mode"`
	Autonomy      string              `json:"autonomy"`
	Lead          string              `json:"lead"`
	OpenTasks     int                 `json:"open_tasks"`
	Tasks         []activeTaskSummary `json:"tasks,omitempty"`
	OpenNotices   int                 `json:"open_notices"`
	Lowkey        bool                `json:"lowkey"`
	ReportingRule string              `json:"reporting_rule"`
	Error         string              `json:"error,omitempty"`
}

func (s *Service) leadStatus(ctx context.Context, project store.Project) string {
	if project.IsDown() {
		return "down"
	}
	if s.Herdr == nil || (project.LeadPaneID == "" && project.LeadLabel == "") {
		return project.Status
	}
	snapshot, err := s.Herdr.Snapshot(ctx)
	if err != nil {
		return project.Status
	}
	for _, pane := range snapshot.Panes {
		if (project.LeadPaneID != "" && pane.PaneID == project.LeadPaneID) || (project.LeadLabel != "" && pane.Label == project.LeadLabel) {
			if pane.AgentStatus != "" {
				return pane.AgentStatus
			}
			if pane.Agent == "" {
				return "idle"
			}
			return "unknown"
		}
	}
	return "missing"
}

type activeTaskSummary struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type taskSummary struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	State   string   `json:"state"`
	Title   string   `json:"title"`
	Name    string   `json:"name"`
	PRState string   `json:"pr_state,omitempty"`
	Profile string   `json:"profile,omitempty"`
	Branch  string   `json:"branch,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Issues  []string `json:"issues,omitempty"`
	Refs    []string `json:"refs,omitempty"`
}

type noticeSummary struct {
	ID      int64  `json:"id"`
	Task    string `json:"task"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

func (s *Service) home(ctx *axi.Context, args []string) error {
	if len(args) > 1 || len(args) == 1 && args[0] != "--full" {
		return axi.Usage("posse accepts only --full", "Run `posse --help` to list commands")
	}
	full := len(args) == 1 && args[0] == "--full"
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if dir, dirErr := currentDir(); dirErr == nil {
		project, found, err := registeredProjectFor(ctx.Context, db, dir)
		if err != nil {
			return err
		}
		if found {
			return s.printProjectHome(ctx, db, project, full)
		}
	}
	projects, err := db.Projects(ctx.Context)
	if err != nil {
		return err
	}
	var summaries []projectSummary
	for _, project := range projects {
		projectName := project.Name
		cfg, err := s.prepareProject(ctx.Context, db, project)
		if err != nil {
			summaries = append(summaries, projectSummary{Name: projectName, Lead: "error", Error: err.Error()})
			continue
		}
		project, err = db.ProjectByID(ctx.Context, project.ID)
		if err != nil {
			summaries = append(summaries, projectSummary{Name: projectName, Lead: "error", Error: err.Error()})
			continue
		}
		tasks, err := db.Tasks(ctx.Context, project.ID, false)
		if err != nil {
			summaries = append(summaries, projectSummary{Name: projectName, Lead: "error", Error: err.Error()})
			continue
		}
		notices, err := db.Notices(ctx.Context, project.ID, true)
		if err != nil {
			summaries = append(summaries, projectSummary{Name: projectName, Lead: "error", Error: err.Error()})
			continue
		}
		summaries = append(summaries, summarizeProject(project, cfg, tasks, len(notices), s.leadStatus(ctx.Context, project)))
	}
	update, _ := s.availableUpdate(ctx.Context, db, nil)
	if len(summaries) == 0 {
		return ctx.Print(addUpdateHelp(axi.Object{
			{Key: "projects", Value: []any{}},
			{Key: "state", Value: "No Projects registered"},
			{Key: "help", Value: []any{"Run `posse up` inside a Herdr pane to register the first Project"}},
		}, update))
	}
	return ctx.Print(addUpdateHelp(axi.Object{
		{Key: "projects", Value: summaries},
		{Key: "help", Value: []any{"Run `posse up` inside a Project to start its Lead", "Run `posse roster --all` to see each Project's Tasks"}},
	}, update))
}

func (s *Service) printProjectHome(ctx *axi.Context, db *store.DB, project store.Project, full bool) error {
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err != nil {
		return err
	}
	project, err = db.ProjectByID(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	tasks, err := db.Tasks(ctx.Context, project.ID, false)
	if err != nil {
		return err
	}
	openNotices, err := db.Notices(ctx.Context, project.ID, true)
	if err != nil {
		return err
	}
	taskRows := make([]taskSummary, 0, len(tasks))
	needsYou := make([]taskSummary, 0)
	for _, task := range tasks {
		row := taskSummary{ID: taskIDString(task.Seq), Type: task.Type, State: string(task.State), Title: task.Title, Name: taskDisplayName(task)}
		if task.PRURL != "" {
			_, row.PRState, err = pullRequestDisplay(ctx.Context, db, task)
			if err != nil {
				return err
			}
		}
		if taskNeedsUser(task) {
			row.Reason, err = taskFailureReason(ctx.Context, db, task)
			if err != nil {
				return err
			}
			needsYou = append(needsYou, row)
		} else {
			taskRows = append(taskRows, row)
		}
	}
	noticeRows := make([]noticeSummary, 0, len(openNotices))
	for _, notice := range openNotices {
		noticeRows = append(noticeRows, noticeSummary{ID: notice.ID, Task: noticeTaskID(ctx.Context, db, project.ID, notice.TaskID), Name: noticeTaskName(ctx.Context, db, project.ID, notice.TaskID), Kind: notice.Kind, Summary: notice.Summary})
	}
	help := homeHelp(tasks, openNotices)
	result := axi.Object{
		{Key: "project", Value: summarizeHomeProject(project, cfg, s.leadStatus(ctx.Context, project))},
	}
	if project.IsWorkspace() {
		targets, err := s.projectTargets(ctx.Context, db, project)
		if err != nil {
			return err
		}
		names := make([]any, 0, len(targets))
		for _, target := range targets {
			names = append(names, target.Name)
		}
		result = append(result, axi.Field{Key: "repos", Value: names})
	}
	result = append(result, axi.Field{Key: "notices", Value: noticeRows}, axi.Field{Key: "tasks", Value: taskRows}, axi.Field{Key: "needs_you", Value: needsYou})
	if !ctx.JSON && !full {
		active := make([]activeTaskSummary, 0, len(tasks))
		attention := make([]taskSummary, 0)
		for _, task := range tasks {
			if taskNeedsUser(task) {
				reason, reasonErr := taskFailureReason(ctx.Context, db, task)
				if reasonErr != nil {
					return reasonErr
				}
				attention = append(attention, taskSummary{ID: taskIDString(task.Seq), Name: taskDisplayName(task), State: string(task.State), Reason: reason})
			} else {
				active = append(active, activeTaskSummary{ID: taskIDString(task.Seq), Name: taskDisplayName(task), State: string(task.State)})
			}
		}
		pending, err := db.UndeliveredNotices(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		result = axi.Object{{Key: "project", Value: project.Name}, {Key: "tasks", Value: active}, {Key: "needs_you", Value: attention}, {Key: "undelivered_notices", Value: len(pending)}, {Key: "next_action", Value: help[0]}}
	}
	update, _ := s.availableUpdate(ctx.Context, db, &project)
	rule := reportingRule(cfg.Lowkey.Lead)
	if !ctx.JSON && !full {
		rule = shortReportingRule(cfg.Lowkey.Lead)
	}
	gaps, err := s.readiness(ctx.Context, db, project, cfg)
	if err != nil {
		return err
	}
	result = withReadiness(result, gaps)
	result = append(result, axi.Field{Key: "lowkey", Value: cfg.Lowkey.Lead}, axi.Field{Key: "reporting_rule", Value: rule})
	if ctx.JSON || full {
		result = append(result, axi.Field{Key: "help", Value: help})
		return ctx.Print(addUpdateHelp(result, update))
	}
	if update != nil {
		return ctx.Print(addUpdateHelp(result, update))
	}
	return ctx.PrintWithoutHelp(result)
}

func summarizeHomeProject(project store.Project, cfg config.Config, leadStatus string) axi.Row {
	autonomy := cfg.Autonomy
	if autonomy.Review == "" {
		autonomy.Review = "ask"
	}
	if autonomy.Land == "" {
		autonomy.Land = "ask"
	}
	if leadStatus == "" {
		leadStatus = "missing"
	}
	return axi.Row{
		{Key: "name", Value: project.Name},
		{Key: "mode", Value: cfg.Defaults.LandingMode},
		{Key: "autonomy", Value: "review=" + autonomy.Review + " land=" + autonomy.Land},
		{Key: "lead", Value: leadStatus},
	}
}

func homeHelp(tasks []store.Task, notices []store.Notice) []any {
	var show, land, recovery string
	for _, task := range tasks {
		if show == "" && (task.State == store.StateNeedsDecision || task.State == store.StateBlocked) {
			show = taskCLIName(task)
		}
		if recovery == "" && taskNeedsUser(task) {
			recovery = taskCLIName(task)
		}
		if land == "" && task.Type == "ship" && (task.State == store.StateDone || task.State == store.StateLanding) {
			land = taskCLIName(task)
		}
	}
	if show == "" {
		show = recovery
	}
	if show == "" && len(tasks) > 0 {
		show = taskCLIName(tasks[0])
	}
	help := []any{}
	if show != "" {
		if show == recovery {
			help = append(help, "Run `posse decisions` to ask the User whether to relaunch or discard "+recovery)
		} else {
			help = append(help, "Run `posse show "+show+"` to inspect this Task")
		}
	} else {
		help = append(help, "Run `posse ride --brief <file> --name <short>` to delegate a Task")
	}
	if land != "" {
		help = append(help, "Run `posse land "+land+"` to Land this completed Ship Task")
	} else {
		help = append(help, "Run `posse roster` to inspect Rider progress")
	}
	if len(notices) > 0 {
		ids := make([]string, len(notices))
		for index, notice := range notices {
			ids[index] = strconv.FormatInt(notice.ID, 10)
		}
		help = append(help, "Run `posse ack "+strings.Join(ids, ",")+"` after handling these Notices")
	} else {
		help = append(help, "Run `posse remuda` to inspect released Mounts")
	}
	return help
}

func shortReportingRule(lowkey bool) string {
	if lowkey {
		return "Report only decisions and completed outcomes; ack routine Notices silently."
	}
	return "Report every Notice and ack it."
}

func summarizeProject(project store.Project, cfg config.Config, tasks []store.Task, noticeCount int, leadStatus string) projectSummary {
	autonomy := cfg.Autonomy
	if autonomy.Review == "" {
		autonomy.Review = "ask"
	}
	if autonomy.Land == "" {
		autonomy.Land = "ask"
	}
	lead := leadStatus
	if lead == "" {
		lead = "missing"
	}
	rows := make([]activeTaskSummary, 0, len(tasks))
	for _, task := range tasks {
		rows = append(rows, activeTaskSummary{ID: project.Name + "/" + taskIDString(task.Seq), Name: taskDisplayName(task), State: string(task.State)})
	}
	return projectSummary{Name: project.Name, Mode: cfg.Defaults.LandingMode, Autonomy: "review=" + autonomy.Review + " land=" + autonomy.Land, Lead: lead, OpenTasks: len(tasks), Tasks: rows, OpenNotices: noticeCount, Lowkey: cfg.Lowkey.Lead, ReportingRule: shortReportingRule(cfg.Lowkey.Lead)}
}

func (s *Service) ls(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("roster", args, map[string]flagSpec{"all": {boolean: true}, "full": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("roster does not take positional arguments")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if parsed.Bool("all") {
		projects, err := db.Projects(ctx.Context)
		if err != nil {
			return err
		}
		var rows []projectSummary
		for _, project := range projects {
			projectName := project.Name
			cfg, err := s.prepareProject(ctx.Context, db, project)
			if err != nil {
				rows = append(rows, projectSummary{Name: projectName, Lead: "error", Error: err.Error()})
				continue
			}
			project, err = db.ProjectByID(ctx.Context, project.ID)
			if err != nil {
				rows = append(rows, projectSummary{Name: projectName, Lead: "error", Error: err.Error()})
				continue
			}
			tasks, err := db.Tasks(ctx.Context, project.ID, false)
			if err != nil {
				rows = append(rows, projectSummary{Name: projectName, Lead: "error", Error: err.Error()})
				continue
			}
			notices, err := db.Notices(ctx.Context, project.ID, true)
			if err != nil {
				rows = append(rows, projectSummary{Name: projectName, Lead: "error", Error: err.Error()})
				continue
			}
			rows = append(rows, summarizeProject(project, cfg, tasks, len(notices), s.leadStatus(ctx.Context, project)))
		}
		if len(rows) == 0 {
			return ctx.Print(axi.Object{{Key: "projects", Value: []any{}}, {Key: "state", Value: "No Projects registered"}, {Key: "help", Value: []any{"Run `posse up` inside a Herdr pane to register the first Project"}}})
		}
		return ctx.Print(axi.Object{{Key: "projects", Value: rows}, {Key: "help", Value: []any{"Run `posse up` inside a Project to start its Lead", "Run `posse roster` to inspect Project Tasks"}}})
	}
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if _, err := s.prepareProject(ctx.Context, db, project); err != nil {
		if !store.IsOnlyBusy(err) {
			return err
		}
		fmt.Fprintln(ctx.ErrOut, "Some Task observations were deferred; showing their last recorded state.")
	}
	tasks, err := db.Tasks(ctx.Context, project.ID, false)
	if err != nil {
		return err
	}
	rows := make([]taskSummary, 0, len(tasks))
	for _, task := range tasks {
		row := taskSummary{ID: taskIDString(task.Seq), Type: task.Type, State: string(task.State), Title: task.Title, Name: taskDisplayName(task), Profile: task.Profile}
		if taskNeedsUser(task) {
			row.Reason, err = taskFailureReason(ctx.Context, db, task)
			if err != nil {
				return err
			}
		}
		if task.PRURL != "" {
			_, row.PRState, err = pullRequestDisplay(ctx.Context, db, task)
			if err != nil {
				return err
			}
		}
		if parsed.Bool("full") {
			row.Issues, row.Refs, err = taskIssueReferences(home, project, task)
			if err != nil {
				return err
			}
			if task.Branch != "posse/"+taskIDString(task.Seq) {
				row.Branch = task.Branch
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return ctx.Print(axi.Object{{Key: "tasks", Value: []any{}}, {Key: "state", Value: "0 open Tasks"}, {Key: "help", Value: []any{"Run `posse ride --brief <file> --name <short>` to delegate a Task"}}})
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "tasks", Value: rows}, {Key: "help", Value: []any{"Run `posse show " + taskCLIName(tasks[0]) + "` to inspect a Task"}}})
}

func (s *Service) show(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("show", args, map[string]flagSpec{"full": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("show requires one Task id", "Run `posse roster` to list Tasks")
	}
	db, home, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLead(ctx.Context, db, project); err != nil {
		return err
	}
	if project.IsWorkspace() {
		if _, err := s.prepareProjectObservation(ctx.Context, db, project); err != nil {
			return err
		}
	} else if _, err := s.prepareProjectInspection(ctx.Context, db, project); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	id := taskIDString(task.Seq)
	view := map[string]any{"id": id, "type": task.Type, "state": string(task.State), "title": task.Title, "name": taskDisplayName(task), "profile": task.Profile}
	if task.Type == "review" {
		reviewerLaunches, err := db.TaskLaunchIdentities(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		reviewerHistoryKnown := launchHistoryComplete(task, reviewerLaunches)
		if task.ReviewsTaskID == 0 {
			view["review_identity"] = map[string]any{"review_task": id, "eligible": false, "reason": "Review Task has no recorded Ship Task", "review_history_known": reviewerHistoryKnown, "review_launches": identityViews(reviewerLaunches)}
		} else {
			author, err := db.TaskByID(ctx.Context, project.ID, task.ReviewsTaskID)
			if err != nil {
				return err
			}
			authorLaunches, err := db.TaskLaunchIdentities(ctx.Context, author.ID)
			if err != nil {
				return err
			}
			authorHistoryKnown := author.Launches > 0 && launchHistoryComplete(author, authorLaunches)
			view["review_identity"] = reviewIdentityHistoryResult(author, authorLaunches, authorHistoryKnown, task, reviewerLaunches, reviewerHistoryKnown)
		}
	}
	recovery, err := db.TaskRecovery(ctx.Context, task.ID)
	if err != nil {
		return err
	}
	if recovery.Status != "" {
		view["recovery"] = recovery
	}
	modelEpisode, err := db.TaskModelErrorEpisode(ctx.Context, task.ID)
	if err != nil {
		return err
	}
	if modelEpisode.Status != "" {
		view["model_error_recovery"] = map[string]any{
			"episode": modelEpisode.Episode, "launch": modelEpisode.Launch, "agent": modelEpisode.Agent,
			"kind": modelEpisode.Kind, "attempts": modelEpisode.Attempts, "max_attempts": modelStreamRetryLimit,
			"status": modelEpisode.Status, "started_at": modelEpisode.StartedAt,
			"next_attempt_at": modelEpisode.NextAttemptAt,
		}
	}
	issues, refs, err := taskIssueReferences(home, project, task)
	if err != nil {
		return err
	}
	if len(issues) > 0 {
		view["issues"] = issues
	}
	if len(refs) > 0 {
		view["refs"] = refs
	}
	if task.Type != "ship" {
		home, err := s.homePath()
		if err != nil {
			return err
		}
		attachments, err := reportAttachments(home, project, task)
		if err != nil {
			return err
		}
		view["attachments"] = attachments
	}
	if task.PRURL != "" {
		pr, _, err := pullRequestDisplay(ctx.Context, db, task)
		if err != nil {
			return err
		}
		view["pr"] = pr
	}
	if project.IsWorkspace() {
		repos, err := db.TaskRepos(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		rows := make([]any, 0, len(repos))
		for _, repo := range repos {
			row := axi.Object{{Key: "repo", Value: repo.Repo}, {Key: "mode", Value: repo.LandingMode}, {Key: "state", Value: repo.State}, {Key: "pr_url", Value: repo.PRURL}, {Key: "pr_state", Value: ""}}
			if repo.PRURL != "" {
				if observation, err := db.LatestMemberPRObservation(ctx.Context, task.ID, repo.Repo); err == nil {
					row[4].Value = observation.State
				} else if !store.IsNotFound(err) {
					return err
				}
			}
			rows = append(rows, row)
		}
		view["repos"] = rows
	}
	commandName := taskCLIName(task)
	help := []any{"Run `posse peek " + commandName + "` to read recent Rider output"}
	if modelEpisode.Status != "" && modelEpisode.Status != "resolved" {
		help = append(help, "Inspect `model_error_recovery` and its Task signals in `posse show "+commandName+" --full`")
	}
	undeliveredMessages, err := db.UndeliveredTaskMessages(ctx.Context, task.ID)
	if err != nil {
		return err
	}
	if len(undeliveredMessages) > 0 {
		rows := make([]any, 0, len(undeliveredMessages))
		hasQueuedMessages := false
		hasUndeliverableMessages := false
		for _, message := range undeliveredMessages {
			rows = append(rows, map[string]any{"id": message.ID, "status": message.Status, "instruction": message.Body})
			switch message.Status {
			case "queued":
				hasQueuedMessages = true
			case "undeliverable":
				hasUndeliverableMessages = true
			}
		}
		view["undelivered_messages"] = rows
		switch task.State {
		case store.StateFailed, store.StateLost:
			help = append(help, "Relaunch with `posse relaunch "+commandName+"`, then resend each undeliverable instruction with `posse send "+commandName+" <message>`")
		case store.StateReported, store.StateLanded, store.StateTornDown:
			help = append(help, "Create a new Ship Task, then resend each undeliverable instruction with `posse send <task> <message>`")
		default:
			if hasUndeliverableMessages {
				help = append(help, "Undeliverable instructions are not retried automatically; resend each with `posse send "+commandName+" <message>`")
			}
			if hasQueuedMessages {
				help = append(help, "Queued instructions will be delivered when the Rider is ready and unfocused")
			}
		}
	}
	if task.Type == "ship" && task.State == store.StateDone {
		help = append(help, "Run `posse land "+commandName+"` to run the Gate")
	}
	if task.State == store.StateNeedsDecision || task.State == store.StateBlocked {
		help = append(help, "Run `posse send "+commandName+" <message>` to answer or steer this Rider")
	}
	if parsed.Bool("full") {
		uncertainMessages, err := db.UncertainTaskMessages(ctx.Context, project.ID, task.ID, currentTime()-deliveryClaimTimeout.Milliseconds())
		if err != nil {
			return err
		}
		if len(uncertainMessages) > 0 {
			rows := make([]any, 0, len(uncertainMessages))
			for _, message := range uncertainMessages {
				rows = append(rows, map[string]any{
					"id": message.ID, "status": "uncertain", "instruction": message.Body,
					"help": "Inspect `posse peek " + commandName + "`; send a replacement only if the instruction is absent",
				})
			}
			view["uncertain_messages"] = rows
			help = append(help, "Inspect uncertain instructions with `posse peek "+commandName+"`; send a replacement only if the instruction is absent")
		}
		signals, err := db.TaskSignals(ctx.Context, task.ID, 5)
		if err != nil {
			return err
		}
		transitions, err := db.TaskTransitions(ctx.Context, task.ID, 10)
		if err != nil {
			return err
		}
		view["signals"] = taskSignalRows(task, signals)
		view["transitions"] = taskTransitionRows(task, transitions)
		view["profile"] = task.Profile
		view["launches"] = task.Launches
		launchIdentities, err := db.TaskLaunchIdentities(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		view["launch_identities"] = identityViews(launchIdentities)
		view["launch_identity_history_known"] = launchHistoryComplete(task, launchIdentities)
		view["dispatch_rule"] = task.DispatchRule
		view["landing_mode"] = task.LandingMode
		if task.Branch != "posse/"+taskIDString(task.Seq) {
			view["branch"] = task.Branch
		}
		if task.BaseRef != "posse/"+taskIDString(task.Seq) {
			view["base_ref"] = task.BaseRef
		}
		view["worktree_path"] = task.WorktreePath
		view["herdr_workspace_id"] = task.HerdrWorkspaceID
		view["pane_id"] = task.PaneID
		view["pr_url"] = task.PRURL
	}
	view["help"] = help
	return ctx.Print(view)
}

func noticeTaskID(ctx context.Context, db *store.DB, projectID, id int64) string {
	if id == 0 {
		return ""
	}
	task, err := db.TaskByID(ctx, projectID, id)
	if err != nil {
		return ""
	}
	return taskIDString(task.Seq)
}

func noticeTaskName(ctx context.Context, db *store.DB, projectID, id int64) string {
	if id == 0 {
		return ""
	}
	task, err := db.TaskByID(ctx, projectID, id)
	if err != nil {
		return ""
	}
	return taskDisplayName(task)
}

func taskNeedsUser(task store.Task) bool {
	return task.State == store.StateFailed || task.State == store.StateLost
}

func taskFailureReason(ctx context.Context, db *store.DB, task store.Task) (string, error) {
	var reason string
	err := db.QueryRowContext(ctx, `SELECT note FROM transitions WHERE task_id=? AND to_state=? AND from_state<>to_state ORDER BY id DESC LIMIT 1`, task.ID, task.State).Scan(&reason)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	return reason, nil
}

func taskSignalRows(task store.Task, signals []store.Signal) []any {
	rows := make([]any, 0, len(signals))
	for _, signal := range signals {
		rows = append(rows, map[string]any{"id": signal.ID, "task_id": taskIDString(task.Seq), "verb": signal.Verb, "note": signal.Note, "data_json": signal.DataJSON, "at": signal.At})
	}
	return rows
}

func taskTransitionRows(task store.Task, transitions []store.TransitionRecord) []any {
	rows := make([]any, 0, len(transitions))
	for _, transition := range transitions {
		rows = append(rows, map[string]any{"id": transition.ID, "task_id": taskIDString(task.Seq), "from": transition.From, "to": transition.To, "source": transition.Source, "note": transition.Note, "at": transition.At})
	}
	return rows
}

func taskDisplayName(task store.Task) string {
	if task.ShortName != "" && !taskIDNamePattern.MatchString(task.ShortName) {
		return task.ShortName
	}
	return task.Title
}

func taskCLIName(task store.Task) string { return taskIDString(task.Seq) }

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func resolveProjectPath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func currentTime() int64 { return time.Now().UnixMilli() }

func writeFile(path string, contents []byte) error {
	return atomicfile.Write(path, contents, 0o600)
}

func oneOfString(value string, items ...string) bool {
	for _, item := range items {
		if value == item {
			return true
		}
	}
	return false
}
