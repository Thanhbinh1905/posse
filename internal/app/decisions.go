package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// raiseNoticeDecisions reconciles persisted Notices into idempotent Decisions.
// It also catches Notices written in the same transaction as a Rider Signal or
// pull request observation, including after a crash before delivery.
func (s *Service) raiseNoticeDecisions(ctx context.Context, db *store.DB, project store.Project) error {
	if err := db.ObsoleteResolvedDecisions(ctx, project.ID); err != nil {
		return err
	}
	notices, err := db.DecisionSourceNotices(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, notice := range notices {
		if notice.TaskID == 0 || !oneOfString(notice.Kind, "land_ready", "task_failed", "task_lost", "task_done", "needs_decision") {
			continue
		}
		task, err := db.TaskByID(ctx, project.ID, notice.TaskID)
		if err != nil {
			return err
		}
		request := store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Origin: fmt.Sprintf("notice:%d", notice.ID)}
		switch {
		case notice.Kind == "land_ready" && task.AutonomyLand != "auto" && task.State == store.StateLanding:
			request.Kind = "land_ready"
			request.Question = "Land " + task.Title + "? " + notice.Summary
			request.Options = []string{"land", "wait"}
		case (notice.Kind == "task_failed" || notice.Kind == "task_lost") && (task.State == store.StateFailed || task.State == store.StateLost):
			request.Kind = "recovery"
			reason, err := taskFailureReason(ctx, db, task)
			if err != nil {
				return err
			}
			request.Question = "Relaunch or discard " + taskIDString(task.Seq) + " (" + taskDisplayName(task) + ")? " + reason
			request.Options = []string{"relaunch", "discard"}
		case notice.Kind == "task_done" && task.Type == "review" && task.ReviewsTaskID != 0 && task.AutonomyReview != "lead":
			request.Kind = "review"
			request.Question = "How should the Review Task findings for " + task.Title + " be handled? " + notice.Summary
			request.Options = []string{"accept", "request-changes", "ignore"}
		case notice.Kind == "needs_decision" && task.LandingMode == "no-mistakes" && task.AutonomyReview != "lead":
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
	if len(notices) > 0 {
		return db.AdvanceDecisionNoticeCursor(ctx, project.ID, notices[len(notices)-1].ID)
	}
	return nil
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
	request := store.DecisionRequest{ProjectID: project.ID, TaskID: task.ID, Origin: "lead:" + strconv.FormatInt(task.ID, 10) + ":" + strconv.FormatInt(time.Now().UnixNano(), 10), Kind: "rider_question", Question: positionals[1], Options: options}
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
	if role == "worker" {
		return axi.Failure("user_only", "a Rider cannot answer a Decision", false)
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
