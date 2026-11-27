package app

import (
	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

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
