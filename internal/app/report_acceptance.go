package app

import (
	"errors"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) acceptReport(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("accept", args, map[string]flagSpec{"user-approved": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("accept requires <task>")
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
	role := s.configCallerRole(ctx.Context, db)
	if role == "worker" {
		return axi.Failure("user_only", "a Worker cannot accept a Report", false)
	}
	if role == "lead" {
		if err := s.requireLead(ctx.Context, db, project); err != nil {
			return err
		}
		if strings.TrimSpace(parsed.Flags["user-approved"]) == "" {
			return axi.Failure("user_only", "the Lead cannot accept a Report without the User's explicit approval", false, "Pass `--user-approved \"<User's exact words>\"` after the User approves this Report")
		}
	}
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.Type != "research" || task.ReportKind != "spec" && task.ReportKind != "tickets" {
		return axi.Failure("report_acceptance_not_required", "only a Research Task marked report_kind spec or tickets uses Report acceptance", false)
	}
	if task.State != store.StateTornDown {
		return axi.Failure("report_bundle_not_finalized", "acceptance requires the Report bundle finalized by Teardown", false, "Run `posse teardown "+taskIDString(task.Seq)+"` to preserve and seal the Report first")
	}
	if err := verifyReportBundle(home, project, task); err != nil {
		return axi.Failure("report_bundle_invalid", err.Error(), false, "Restore the exact Report and attachment bytes, then inspect `posse show "+taskIDString(task.Seq)+"`")
	}
	recordedBy := role
	acceptance, err := db.AcceptReport(ctx.Context, task.ID, task.ReportDigest, recordedBy, parsed.Flags["user-approved"])
	if errors.Is(err, store.ErrIndependentReportRequired) {
		return axi.Failure("report_review_required", err.Error(), false, "A current independent Report-review pass must target this exact Report digest before acceptance")
	}
	if err != nil {
		return axi.Failure("report_acceptance_refused", err.Error(), false)
	}
	return ctx.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)},
		{Key: "report_digest", Value: acceptance.ReportDigest},
		{Key: "accepted_by", Value: acceptance.AcceptedBy},
		{Key: "recorded_by", Value: acceptance.RecordedBy},
		{Key: "provenance", Value: acceptance.Provenance},
		{Key: "user_quote", Value: acceptance.UserQuote},
		{Key: "accepted_at", Value: acceptance.AcceptedAt},
		{Key: "help", Value: []any{"The exact Report bundle and User acceptance are recorded; inspect `posse show " + taskIDString(task.Seq) + "` to verify provenance"}},
	})
}
