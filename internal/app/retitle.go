package app

import (
	"regexp"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

var conventionalPRTitle = regexp.MustCompile(`^[a-z][a-z0-9-]*(?:\([^\s()]+\))?!?:\s+\S.*$`)

func validConventionalPRTitle(title string) bool {
	return !strings.ContainsAny(title, "\r\n") && conventionalPRTitle.MatchString(strings.TrimSpace(title))
}

func (s *Service) retitle(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("retitle", args, nil)
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 2 {
		return axi.Usage("retitle requires a Task id and conventional PR title")
	}
	title := strings.TrimSpace(parsed.Positionals[1])
	if !validConventionalPRTitle(title) {
		return axi.Failure("pr_title_invalid", "PR title must use conventional-commit form type[(scope)][!]: description", false, "Use a lowercase type and a non-empty description, such as `fix(publish): preserve the retitled PR`")
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
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.Type != "ship" || task.LandingMode != "pr" {
		return axi.Failure("retitle_refused", "only PR-mode Ship Tasks can have a publish title", false)
	}
	if task.State == store.StateLanded || task.State == store.StateReported || task.State == store.StateTornDown {
		return axi.Failure("retitle_refused", "a completed Task cannot be retitled", false, "Retitle an open PR before the Task Lands")
	}
	if task.PRTitle != title {
		if err := db.SetTaskPRTitle(ctx.Context, task.ID, title); err != nil {
			return err
		}
	}
	return ctx.Print(axi.Object{
		{Key: "task", Value: taskIDString(task.Seq)},
		{Key: "pr_title", Value: title},
		{Key: "help", Value: []any{"The Rider's next `posse publish` will use this title"}},
	})
}
