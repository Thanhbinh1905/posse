package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// raiseNoticeDecisions reconciles persisted Notices into idempotent Decisions.
// It also catches Notices written in the same transaction as a Worker Signal or
// pull request observation, including after a crash before delivery.
func (s *Service) raiseNoticeDecisions(ctx context.Context, db *store.DB, project store.Project) error {
	if err := db.ObsoleteResolvedDecisions(ctx, project.ID); err != nil {
		return err
	}
	if err := s.obsoleteEmptyOrphans(ctx, db, project); err != nil {
		return err
	}
	notices, err := db.DecisionSourceNotices(ctx, project.ID)
	if err != nil {
		return err
	}
	capNotices, err := db.ReviewCapNotices(ctx, project.ID)
	if err != nil {
		return err
	}
	byID := make(map[int64]store.Notice, len(notices)+len(capNotices))
	for _, notice := range notices {
		byID[notice.ID] = notice
	}
	latestCapNotice := map[int64]int64{}
	for _, notice := range capNotices {
		byID[notice.ID] = notice
		if notice.ID > latestCapNotice[notice.TaskID] {
			latestCapNotice[notice.TaskID] = notice.ID
		}
	}
	notices = notices[:0]
	for _, notice := range byID {
		notices = append(notices, notice)
	}
	sort.Slice(notices, func(i, j int) bool { return notices[i].ID < notices[j].ID })
	for _, notice := range notices {
		if notice.Kind == "review_cap_reached" && latestCapNotice[notice.TaskID] != notice.ID {
			continue
		}
		if notice.TaskID == 0 || !oneOfString(notice.Kind, "merge_ready", "task_failed", "task_lost", "task_done", "needs_decision", "worker_exited", "review_cap_reached") {
			continue
		}
		task, err := db.TaskByID(ctx, project.ID, notice.TaskID)
		if err != nil {
			return err
		}
		if notice.Kind == "worker_exited" {
			if err := s.raiseWorkerExitedDecision(ctx, db, project, task, notice); err != nil {
				return err
			}
			continue
		}
		request := store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Origin: fmt.Sprintf("notice:%d", notice.ID)}
		switch {
		case notice.Kind == "review_cap_reached" && task.Type == "change":
			head, used, max, err := reviewCapNoticeData(notice)
			if err != nil {
				return err
			}
			if err := db.ObsoleteStaleReviewCapDecisions(ctx, task.ID, reviewCapOrigin(task, head)); err != nil {
				return err
			}
			request.Kind = "review_cap"
			request.Origin = reviewCapOrigin(task, head)
			request.Question = fmt.Sprintf("Review and repair cap reached for %s at %s (%d of %d Review/repair rounds used). Choose how to proceed.", taskIDString(task.Seq), shortSHA(head), used, max)
			request.Options = []string{"accept-head", "final-fix", "rethink", "discard"}
		case notice.Kind == "merge_ready" && task.AutonomyMerge != "auto" && task.State == store.StateMerging:
			request.Kind = "merge_ready"
			request.Question = "Merge " + task.Title + "? " + notice.Summary
			request.Options = []string{"merge", "wait", "discard"}
		case (notice.Kind == "task_failed" || notice.Kind == "task_lost") && (task.State == store.StateFailed || task.State == store.StateLost):
			request.Kind = "recovery"
			reason, err := taskFailureReason(ctx, db, task)
			if err != nil {
				return err
			}
			plan, err := task.AdmittedPlan()
			if err != nil {
				return admittedPlanFailure(err)
			}
			if plan != nil && plan.Execution.Mode == "in-place" {
				request.Question = "Relaunch or preserve and stop " + taskIDString(task.Seq) + " (" + taskDisplayName(task) + ")? " + reason
				request.Options = []string{"relaunch", "preserve"}
			} else {
				request.Question = "Relaunch or discard " + taskIDString(task.Seq) + " (" + taskDisplayName(task) + ")? " + reason
				request.Options = []string{"relaunch", "discard"}
			}
		case notice.Kind == "task_done" && task.Type == "review" && task.ReviewsTaskID != 0 && task.AutonomyReview != "lead":
			request.Kind = "review"
			request.Question = "How should the Review Task findings for " + task.Title + " be handled? " + notice.Summary
			request.Options = []string{"accept", "request-changes", "ignore"}
		case notice.Kind == "needs_decision" && task.MergeMode == "no-mistakes" && task.AutonomyReview != "lead":
			home, err := s.homePath()
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "findings.toon")); err == nil {
				request.Kind = "review"
				request.Question = "How should the findings for " + task.Title + " be handled? " + notice.Summary
				request.Options = []string{"accept", "request-changes", "ignore"}
			}
		}
		if request.Question != "" {
			if _, err := db.RaiseDecision(ctx, request); err != nil {
				return err
			}
		}
	}
	// Older versions advanced the cursor past worker_exited Notices without
	// creating a Decision. Reconcile those notices without rescanning others.
	unanswered, err := db.UnansweredWorkerExitedNotices(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, notice := range unanswered {
		task, err := db.TaskByID(ctx, project.ID, notice.TaskID)
		if err != nil {
			return err
		}
		if err := s.raiseWorkerExitedDecision(ctx, db, project, task, notice); err != nil {
			return err
		}
	}
	if len(notices) > 0 {
		return db.AdvanceDecisionNoticeCursor(ctx, project.ID, notices[len(notices)-1].ID)
	}
	return nil
}

func (s *Service) raiseWorkerExitedDecision(ctx context.Context, db *store.DB, project store.Project, task store.Task, notice store.Notice) error {
	if task.State != store.StateDone && task.State != store.StateMerging {
		return nil
	}
	var data struct {
		TaskLaunches *int `json:"task_launches"`
	}
	if notice.DataJSON != "" {
		if err := json.Unmarshal([]byte(notice.DataJSON), &data); err != nil {
			return err
		}
	}
	if data.TaskLaunches != nil {
		if *data.TaskLaunches != task.Launches {
			return nil
		}
	} else if task.Launches != 1 {
		// Legacy Notices have no launch identity. They are safe to backfill only
		// when the Task has never been relaunched.
		return nil
	}
	_, err := db.RaiseDecision(ctx, store.DecisionRequest{
		ProjectID: project.ID,
		TaskID:    task.ID,
		Origin:    fmt.Sprintf("notice:%d", notice.ID),
		Kind:      "worker_exited",
		Question:  "Worker exited after reporting " + string(task.State) + " for " + task.Title + ". Relaunch or discard? " + notice.Summary,
		Options:   []string{"relaunch", "discard"},
	})
	return err
}

func (s *Service) ask(ctx *axi.Context, args []string) error {
	// --option is repeatable; the generic flag parser keeps only the last value.
	var options []string
	var positionals []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--option" {
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
				return axi.Usage("--option requires a value")
			}
			i++
			options = append(options, args[i])
		} else if value, ok := strings.CutPrefix(args[i], "--option="); ok {
			options = append(options, value)
		} else if strings.HasPrefix(args[i], "--") {
			return axi.Usage("unknown ask flag " + args[i])
		} else {
			positionals = append(positionals, args[i])
		}
	}
	if len(positionals) != 2 || len(options) < 2 {
		return axi.Usage("ask requires <task> <question> and at least two --option values")
	}
	db, _, err := s.openDB()
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
	task, err := s.currentTask(ctx.Context, db, project, positionals[0])
	if err != nil {
		return err
	}
	if task.State == store.StateTornDown {
		return axi.Failure("ask_refused", "Task is torn down", false)
	}
	request := store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Origin: "lead:" + strconv.FormatInt(task.ID, 10) + ":" + strconv.FormatInt(time.Now().UnixNano(), 10), Kind: "worker_question", Question: positionals[1], Options: options}
	decision, err := db.RaiseDecision(ctx.Context, request)
	if err != nil {
		return axi.Usage(err.Error())
	}
	return ctx.Print(axi.Object{{Key: "decision", Value: decisionOutput(ctx.Context, db, project.ID, decision)}, {Key: "help", Value: []any{"Ask the User to choose an option; record their answer with `posse decide <decision> <option> --user-approved \"<User's words>\"`"}}})
}

func (s *Service) decisions(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("decisions", args, map[string]flagSpec{"all": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("decisions takes no positional arguments")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLeadOrUser(ctx.Context, db, project); err != nil {
		return err
	}
	if err := s.raiseNoticeDecisions(ctx.Context, db, project); err != nil {
		return err
	}
	items, err := db.Decisions(ctx.Context, project.ID, !parsed.Bool("all"))
	if err != nil {
		return err
	}
	rows := make([]any, 0, len(items))
	for _, item := range items {
		rows = append(rows, decisionOutput(ctx.Context, db, project.ID, item))
	}
	return ctx.Print(axi.Object{{Key: "decisions", Value: rows}, {Key: "help", Value: []any{"Run `posse decide <decision> <option> --user-approved \"<User's words>\"` to answer a Decision"}}})
}

func decisionOutput(ctx context.Context, db *store.DB, projectID int64, d store.Decision) map[string]any {
	return map[string]any{
		"id": d.ID, "project_id": d.ProjectID, "task_id": noticeTaskID(ctx, db, projectID, d.TaskID),
		"origin": d.Origin, "question": d.Question, "options": d.Options,
		"answer": d.Answer, "user_quote": d.UserQuote, "created_at": d.CreatedAt,
		"answered_at": d.AnsweredAt, "kind": d.Kind, "task_launches": d.TaskLaunches,
		"obsolete_at": d.ObsoleteAt, "obsolete_reason": d.ObsoleteReason,
	}
}

func (s *Service) requireLeadOrUser(ctx context.Context, db *store.DB, project store.Project) error {
	role := s.configCallerRole(ctx, db)
	if role == "worker" || role == "agent" {
		return axi.Failure("user_only", "a Worker or unbound agent cannot answer a Decision", false)
	}
	if role == "lead" {
		return s.requireLead(ctx, db, project)
	}
	return nil
}

func (s *Service) decide(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("decide", args, map[string]flagSpec{"user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 2 {
		return axi.Usage("decide requires <decision> <option>")
	}
	id, err := strconv.ParseInt(parsed.Positionals[0], 10, 64)
	if err != nil || id < 1 {
		return axi.Usage("Decision id must be a positive number")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	project, err := s.projectForCWD(ctx.Context, db)
	if err != nil {
		return err
	}
	if err := s.requireLeadOrUser(ctx.Context, db, project); err != nil {
		return err
	}
	quote := parsed.Flags["user-approved"]
	if s.configCallerRole(ctx.Context, db) == "lead" && strings.TrimSpace(quote) == "" {
		return axi.Failure("user_only", "only the User answers a Decision; the Lead must quote the User", false, "Pass `--user-approved \"<User's words>\"` after the User answers")
	}
	if quote == "" {
		quote = parsed.Positionals[1]
	} // User's own shell input is their quote.
	decision, err := db.AnswerDecision(ctx.Context, project.ID, id, parsed.Positionals[1], quote)
	if store.IsNotFound(err) {
		return axi.Failure("decision_unknown", "Decision not found in this Project", false, "Run `posse decisions` to list pending Decisions")
	}
	if err != nil {
		return axi.Failure("decision_refused", err.Error(), false, "Run `posse decisions` to inspect valid options")
	}
	if err := s.deliverNotices(ctx.Context, db, project); err != nil && !isHerdrUnavailable(err) {
		return err
	}
	return ctx.Print(axi.Object{{Key: "decision", Value: decisionOutput(ctx.Context, db, project.ID, decision)}, {Key: "help", Value: []any{"The Lead will receive a Notice to carry out the chosen action"}}})
}
