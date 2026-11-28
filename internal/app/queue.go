package app

import (
	"fmt"
	"strings"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) reviseQueuedPrerequisites(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("queue prerequisites", args, map[string]flagSpec{"after": {repeatable: true}, "clear": {boolean: true}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 || parsed.Bool("clear") == (len(parsed.Repeated["after"]) > 0) {
		return axi.Usage("queue prerequisites requires <task> with one or more --after <task:milestone> values, or --clear")
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
	task, err := s.currentTask(ctx.Context, db, project, parsed.Positionals[0])
	if err != nil {
		return err
	}
	if task.State != store.StateQueued {
		return axi.Failure("task_not_queued", "Task is "+string(task.State)+", not queued", false)
	}
	plan, err := task.AdmittedPlan()
	if err != nil {
		return admittedPlanFailure(err)
	}
	if plan == nil {
		return axi.Failure("admitted_plan_missing", "queued Task has no admitted plan to revise", false)
	}
	dependencies := make([]dispatch.Dependency, 0, len(parsed.Repeated["after"]))
	for _, value := range parsed.Repeated["after"] {
		taskRef, milestone, ok := strings.Cut(value, ":")
		if !ok || taskRef == "" || milestone == "" || strings.Contains(milestone, ":") {
			return axi.Usage(fmt.Sprintf("invalid prerequisite %q; expected <task:milestone>", value))
		}
		dependencies = append(dependencies, dispatch.Dependency{TaskID: taskRef, Milestone: milestone})
	}
	revised, err := plan.WithDependencies(dependencies)
	if err != nil {
		return axi.Usage(err.Error())
	}
	if err := db.ReviseTaskPlan(ctx.Context, task, revised); err != nil {
		return err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	if _, err := s.refillQueuedTasks(ctx.Context, db, project, cfg, false); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "plan_revision", Value: revised.Revision}, {Key: "prerequisites", Value: dependencies}})
}

func (s *Service) moveQueuedTask(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("queue move", args, nil)
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 1 {
		return axi.Usage("queue move requires <task>")
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
	if task.State != store.StateQueued {
		return axi.Failure("task_not_queued", "Task is "+string(task.State)+", not queued", false)
	}
	if err := db.MoveQueuedTaskToFront(ctx.Context, project.ID, task.ID); err != nil {
		return err
	}
	return ctx.Print(axi.Object{{Key: "task", Value: taskIDString(task.Seq)}, {Key: "state", Value: "queued"}, {Key: "position", Value: 1}})
}
