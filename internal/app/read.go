package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
	"github.com/thanhbinh1905/posse/internal/usage"
)

type projectSummary struct {
	Name          string              `json:"name"`
	Mode          string              `json:"mode"`
	Autonomy      string              `json:"autonomy"`
	Lead          string              `json:"lead"`
	Watch         string              `json:"watch"`
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
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	MergeQueue string `json:"merge_queue,omitempty"`
}

type taskSummary struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	State   string       `json:"state"`
	Title   string       `json:"title"`
	Name    string       `json:"name"`
	PRState string       `json:"pr_state,omitempty"`
	Queue   string       `json:"merge_queue,omitempty"`
	Profile string       `json:"profile,omitempty"`
	Branch  string       `json:"branch,omitempty"`
	Reason  string       `json:"reason,omitempty"`
	Issues  []string     `json:"issues,omitempty"`
	Refs    []string     `json:"refs,omitempty"`
	Usage   usage.Counts `json:"usage"`
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
		err = deferObservationContention(ctx, err, "Project observations were deferred; showing the last recorded state.")
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
		summary := summarizeProject(project, cfg, tasks, len(notices), s.leadStatus(ctx.Context, project))
		home, homeErr := s.homePath()
		if homeErr == nil {
			summary.Watch = s.watchRuntimeSummary(ctx.Context, db, home, project)
		}
		summaries = append(summaries, summary)
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
		{Key: "help", Value: []any{"Run `posse up` inside a Project to start its Lead", "Run `posse list --all` to see each Project's Tasks"}},
	}, update))
}

func (s *Service) printProjectHome(ctx *axi.Context, db *store.DB, project store.Project, full bool) error {
	cfg, err := s.prepareProject(ctx.Context, db, project)
	err = deferObservationContention(ctx, err, "Project observations were deferred; showing the last recorded state.")
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
	queuePlaces, err := db.MergeQueuePlaces(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	taskRows := make([]taskSummary, 0, len(tasks))
	needsYou := make([]taskSummary, 0)
	for _, task := range tasks {
		row := taskSummary{ID: taskIDString(task.Seq), Type: task.Type, State: string(task.State), Title: task.Title, Name: taskDisplayName(task), Queue: mergeQueueLabel(queuePlaces[task.ID])}
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
	help := homeHelp(project, tasks, openNotices)
	home, homeErr := s.homePath()
	if homeErr != nil {
		return homeErr
	}
	result := axi.Object{
		{Key: "project", Value: summarizeHomeProject(project, cfg, s.leadStatus(ctx.Context, project))},
		{Key: "watch", Value: s.watchRuntimeRow(ctx.Context, db, home, project)},
	}
	if project.IsWorkspace() {
		targets, err := projectTargets(ctx.Context, db, project)
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
				active = append(active, activeTaskSummary{ID: taskIDString(task.Seq), Name: taskDisplayName(task), State: string(task.State), MergeQueue: mergeQueueLabel(queuePlaces[task.ID])})
			}
		}
		pending, err := db.UndeliveredNotices(ctx.Context, project.ID)
		if err != nil {
			return err
		}
		result = axi.Object{{Key: "project", Value: project.Name}, {Key: "watch", Value: s.watchRuntimeRow(ctx.Context, db, home, project)}, {Key: "tasks", Value: active}, {Key: "needs_you", Value: attention}, {Key: "undelivered_notices", Value: len(pending)}, {Key: "next_action", Value: help[0]}}
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
	if autonomy.Merge == "" {
		autonomy.Merge = "ask"
	}
	if leadStatus == "" {
		leadStatus = "missing"
	}
	return axi.Row{
		{Key: "name", Value: project.Name},
		{Key: "mode", Value: projectMergeMode(project, cfg)},
		{Key: "autonomy", Value: "review=" + autonomy.Review + " merge=" + autonomy.Merge},
		{Key: "lead", Value: leadStatus},
	}
}

func homeHelp(project store.Project, tasks []store.Task, notices []store.Notice) []any {
	var show, merge, recovery string
	for _, task := range tasks {
		if show == "" && (task.State == store.StateNeedsDecision || task.State == store.StateBlocked) {
			show = taskCLIName(task)
		}
		if recovery == "" && taskNeedsUser(task) {
			recovery = taskCLIName(task)
		}
		if merge == "" && task.Type == "change" && (task.State == store.StateDone || task.State == store.StateMerging) {
			merge = taskCLIName(task)
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
			choice := "discard"
			if project.IsDirectory() {
				choice = "preserve and stop"
			}
			help = append(help, "Run `posse decisions` to ask the User whether to relaunch or "+choice+" "+recovery)
		} else {
			help = append(help, "Run `posse show "+show+"` to inspect this Task")
		}
	} else if project.IsDirectory() {
		help = append(help, "Directory Projects no longer admit Tasks; register a Git repository instead")
	} else {
		help = append(help, "Run `posse start --brief <file> [--name <short>]` to delegate a Task")
	}
	if merge != "" {
		help = append(help, "Run `posse merge "+merge+"` to Merge this completed Change Task")
	} else {
		help = append(help, "Run `posse list` to inspect Worker progress")
	}
	if len(notices) > 0 {
		ids := make([]string, len(notices))
		for index, notice := range notices {
			ids[index] = strconv.FormatInt(notice.ID, 10)
		}
		help = append(help, "Run `posse ack "+strings.Join(ids, ",")+"` after handling these Notices")
	} else if project.IsDirectory() {
		help = append(help, "Legacy Directory Projects do not admit new Tasks")
	} else {
		help = append(help, "Run `posse pool` to inspect released Sandboxes")
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
	if autonomy.Merge == "" {
		autonomy.Merge = "ask"
	}
	lead := leadStatus
	if lead == "" {
		lead = "missing"
	}
	rows := make([]activeTaskSummary, 0, len(tasks))
	for _, task := range tasks {
		rows = append(rows, activeTaskSummary{ID: project.Name + "/" + taskIDString(task.Seq), Name: taskDisplayName(task), State: string(task.State)})
	}
	return projectSummary{Name: project.Name, Mode: projectMergeMode(project, cfg), Autonomy: "review=" + autonomy.Review + " merge=" + autonomy.Merge, Lead: lead, OpenTasks: len(tasks), Tasks: rows, OpenNotices: noticeCount, Lowkey: cfg.Lowkey.Lead, ReportingRule: shortReportingRule(cfg.Lowkey.Lead)}
}

func (s *Service) ls(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("list", args, map[string]flagSpec{"all": {boolean: true}, "full": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("list does not take positional arguments")
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
			err = deferObservationContention(ctx, err, "Project observations were deferred; showing the last recorded state.")
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
		return ctx.Print(axi.Object{{Key: "projects", Value: rows}, {Key: "help", Value: []any{"Run `posse up` inside a Project to start its Lead", "Run `posse list` to inspect Project Tasks"}}})
	}
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	cfg, err := s.prepareProject(ctx.Context, db, project)
	if err = deferObservationContention(ctx, err, "Some Task observations were deferred; showing their last recorded state."); err != nil {
		return err
	}
	tasks, err := db.Tasks(ctx.Context, project.ID, false)
	if err != nil {
		return err
	}
	queuePlaces, err := db.MergeQueuePlaces(ctx.Context, project.ID)
	if err != nil {
		return err
	}
	rows := make([]taskSummary, 0, len(tasks))
	for _, task := range tasks {
		row := taskSummary{ID: taskIDString(task.Seq), Type: task.Type, State: string(task.State), Title: task.Title, Name: taskDisplayName(task), Queue: mergeQueueLabel(queuePlaces[task.ID]), Profile: task.Profile, Usage: taskUsage(task, cfg)}
		if taskNeedsUser(task) {
			row.Reason, err = taskFailureReason(ctx.Context, db, task)
			if err != nil {
				return err
			}
		} else if task.State == store.StateQueued {
			row.Reason, err = queuedTaskWaitReason(ctx.Context, db, project, task, cfg)
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
			if task.Branch != store.TaskBranchName(task.Seq) {
				row.Branch = task.Branch
			}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return ctx.Print(axi.Object{{Key: "tasks", Value: []any{}}, {Key: "state", Value: "0 open Tasks"}, {Key: "help", Value: []any{"Run `posse start --brief <file> [--name <short>]` to delegate a Task"}}})
	}
	return ctx.Print(axi.Object{{Key: "project", Value: project.Name}, {Key: "tasks", Value: rows}, {Key: "help", Value: []any{"Run `posse show " + taskCLIName(tasks[0]) + "` to inspect a Task"}}})
}

func queuedTaskWaitReason(ctx context.Context, db *store.DB, project store.Project, task store.Task, cfg config.Config) (string, error) {
	plan, err := task.AdmittedPlan()
	if err != nil {
		return "", admittedPlanFailure(err)
	}
	if plan != nil && len(plan.Dependencies) > 0 {
		unmet, err := db.UnmetTaskDependencies(ctx, project.ID, plan.Dependencies)
		if err != nil {
			return "", err
		}
		if len(unmet) > 0 {
			return "waiting for " + strings.Join(unmet, ", "), nil
		}
	}
	blockerReason, err := queuedTaskBlockerWaitReason(ctx, db, project, cfg, plan)
	if err != nil {
		return "", err
	}
	if blockerReason != "" {
		return "waiting for " + blockerReason, nil
	}
	return "waiting for a Worker slot", nil
}

func taskUsage(task store.Task, cfg config.Config) usage.Counts {
	kind := task.AgentSessionKind
	if kind == "" {
		if plan, err := task.AdmittedPlan(); err == nil && plan != nil {
			kind = plan.Launch.Profile.Kind
		} else if profile, ok := cfg.Profiles[task.Profile]; ok {
			kind = profile.Kind
		}
	}
	return usage.Read(kind, agentSessionID(kind, task.AgentSession), task.WorktreePath)
}

func (s *Service) show(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("show", args, map[string]flagSpec{"full": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("show requires one Task id", "Run `posse list` to list Tasks")
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
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	var preparationErr error
	if project.IsWorkspace() {
		_, preparationErr = s.prepareProjectObservation(ctx.Context, db, project)
	} else {
		_, preparationErr = s.prepareProjectInspection(ctx.Context, db, project)
	}
	if err := deferObservationContention(ctx, preparationErr, "Project observations were deferred; showing the last recorded state."); err != nil {
		return err
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	id := taskIDString(task.Seq)
	view := map[string]any{"id": id, "type": task.Type, "state": string(task.State), "title": task.Title, "name": taskDisplayName(task), "profile": task.Profile, "usage": taskUsage(task, cfg)}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	view["review_evidence"] = reviewEvidenceStatus(task)
	view["admission"] = "legacy"
	if plan != nil {
		view["admission"], view["plan_revision"] = "admitted", plan.Revision
		view["execution_mode"] = plan.Execution.Mode
		if task.State == store.StateQueued {
			waitReason, err := queuedTaskWaitReason(ctx.Context, db, project, task, cfg)
			if err != nil {
				return err
			}
			view["wait_reason"] = waitReason
		}
		if parsed.Bool("full") {
			view["plan"] = plan
			if !ctx.JSON {
				readable, err := planWithTextInputs(plan)
				if err != nil {
					return err
				}
				view["plan"] = readable
			}
		}
	}
	if taskNeedsUser(task) {
		reason, err := taskFailureReason(ctx.Context, db, task)
		if err != nil {
			return err
		}
		if reason != "" {
			view["reason"] = reason
		}
	}
	if children, err := taskResearchViews(ctx.Context, db, task); err != nil {
		return err
	} else if len(children) > 0 {
		view["research_children"] = children
	}
	if task.Type == "research" {
		view["report_digest"] = task.ReportDigest
		if task.ReportKind != "" {
			view["report_kind"] = task.ReportKind
		}
		reviews, err := db.ReportReviewsForTask(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		if len(reviews) > 0 {
			view["report_reviews"] = reviews
		}
		acceptance, err := db.ReportAcceptanceForTask(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		if acceptance != nil {
			view["report_acceptance"] = acceptance
		}
	}
	if task.Type == "review" {
		reviewerLaunches, err := db.TaskLaunchIdentities(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		reviewerHistoryKnown := launchHistoryComplete(task, reviewerLaunches)
		if task.ReviewsTaskID == 0 {
			view["review_identity"] = map[string]any{"review_task": id, "eligible": false, "reason": "Review Task has no recorded Change Task", "review_history_known": reviewerHistoryKnown, "review_launches": identityViews(reviewerLaunches)}
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
		if verdict, err := db.ReviewVerdict(ctx.Context, task.ID); err == nil {
			current := false
			if author, authorErr := db.TaskByID(ctx.Context, project.ID, task.ReviewsTaskID); authorErr == nil {
				if pass, ok, passErr := currentReviewPass(ctx.Context, db, project, author); passErr == nil && ok && pass.TaskID == verdict.TaskID {
					current = true
				}
			}
			view["review_verdict"] = reviewVerdictView(verdict, current)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if task.Type == "change" {
		validation, err := validationSummaryFor(ctx.Context, db, task)
		if err != nil {
			return err
		}
		view["validation"] = validation.view()
		receipts, err := db.ReviewReceipts(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		if len(receipts) > 0 {
			views := make([]map[string]any, 0, len(receipts))
			for _, receipt := range receipts {
				views = append(views, map[string]any{"repository": receipt.Repository, "run_id": receipt.RunID, "head_sha": receipt.HeadSHA, "review_status": receipt.ReviewStatus, "reviewer_profile": receipt.ReviewerProfile, "reviewer_model": receipt.ReviewerModel, "report_sha256": receipt.ReportSHA256, "receipt_sha256": receipt.ReceiptSHA256})
			}
			view["review_receipts"] = views
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
	if task.Type != "change" {
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
	if queue, err := mergeQueueView(ctx.Context, db, task); err != nil {
		return err
	} else if len(queue) > 0 {
		view["merge_queue"] = queue
	}
	if project.IsWorkspace() {
		repos, err := db.TaskRepos(ctx.Context, task.ID)
		if err != nil {
			return err
		}
		rows := make([]any, 0, len(repos))
		for _, repo := range repos {
			row := axi.Object{{Key: "repo", Value: repo.Repo}, {Key: "mode", Value: repo.MergeMode}, {Key: "state", Value: repo.State}, {Key: "pr_url", Value: repo.PRURL}, {Key: "pr_state", Value: ""}, {Key: "target_branch", Value: repo.TargetBranch}}
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
	help := []any{"Run `posse peek " + commandName + "` to read recent Worker output"}
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
		hasHistoricalMessages := false
		queuedAt := currentTime()
		for _, message := range undeliveredMessages {
			row := map[string]any{"id": message.ID, "status": message.Status, "instruction": message.Body}
			if message.Status == "queued" {
				age := time.Duration(max(int64(0), queuedAt-message.CreatedAt)) * time.Millisecond
				row["queued_for"] = age.Round(time.Second).String()
			}
			switch message.Status {
			case "queued":
				hasQueuedMessages = true
			case "undeliverable":
				hasUndeliverableMessages = true
				hasHistoricalMessages = true
			case "withdrawn":
				hasHistoricalMessages = true
			}
			if message.Status == "undeliverable" || message.Status == "withdrawn" {
				row["history"] = true
				row["blocks_sends"] = false
			}
			rows = append(rows, row)
		}
		view["undelivered_messages"] = rows
		switch task.State {
		case store.StateFailed, store.StateLost:
			help = append(help, "Relaunch with `posse relaunch "+commandName+"`, then resend each undeliverable instruction with `posse send "+commandName+" <message>`")
		case store.StateReported, store.StateMerged, store.StateTornDown:
			help = append(help, "Create a new Change Task, then resend each undeliverable instruction with `posse send <task> <message>`")
		default:
			if hasUndeliverableMessages {
				help = append(help, "Undeliverable instructions are not retried automatically; resend each with `posse send "+commandName+" <message>`")
			}
			if hasQueuedMessages {
				help = append(help, "Queued instructions will be delivered when the Worker is ready and unfocused")
			}
		}
		if hasHistoricalMessages {
			help = append(help, "Undeliverable and withdrawn messages are history, not pending deliveries; they do not block new sends when this Task accepts instructions")
		}
	}
	if task.Type == "change" && task.State == store.StateDone {
		help = append(help, "Run `posse merge "+commandName+"` to record the merge head and check delivery readiness")
	}
	if task.State == store.StateNeedsDecision || task.State == store.StateBlocked {
		help = append(help, "Run `posse send "+commandName+" <message>` to answer or steer this Worker")
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
		view["merge_mode"] = task.MergeMode
		if task.Type == "change" && !project.IsWorkspace() && !project.IsDirectory() {
			view["target_branch"] = taskTargetBranch(project, task)
		}
		if task.Branch != "" && task.Branch != store.TaskBranchName(task.Seq) {
			view["branch"] = task.Branch
		}
		if task.BaseRef != "" && task.BaseRef != store.TaskBranchName(task.Seq) {
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

// planWithTextInputs renders each frozen input as text so `show --full` is
// readable; input bytes that are not valid UTF-8 stay base64 under bytes_base64.
func planWithTextInputs(plan *taskplan.Plan) (any, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	view, err := axi.ParseOrderedJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	root, ok := view.(axi.Object)
	if !ok {
		return view, nil
	}
	for _, field := range root {
		inputs, ok := field.Value.([]any)
		if field.Key != "inputs" || !ok {
			continue
		}
		for _, item := range inputs {
			input, ok := item.(axi.Object)
			if !ok {
				continue
			}
			for index, entry := range input {
				if entry.Key != "bytes" {
					continue
				}
				encoded, _ := entry.Value.(string)
				raw, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					continue
				}
				if utf8.Valid(raw) {
					input[index] = axi.Field{Key: "text", Value: string(raw)}
				} else {
					input[index] = axi.Field{Key: "bytes_base64", Value: encoded}
				}
			}
		}
	}
	return root, nil
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
