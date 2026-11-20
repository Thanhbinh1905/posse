package app

import (
	"context"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

const projectParkGrace = time.Hour

func projectHasActiveWorker(tasks []store.Task) bool {
	for _, task := range tasks {
		if task.State == store.StateSpawning || task.State == store.StateWorking || task.State == store.StateBlocked || task.State == store.StateStalled || task.AgentStatus == "working" {
			return true
		}
	}
	return false
}

func projectReadyToPark(project store.Project, tasks []store.Task, now time.Time, grace time.Duration) bool {
	return !project.IsDown() && !project.IsParked() && project.LeadAbsentSince > 0 && !projectHasActiveWorker(tasks) && now.UnixMilli()-project.LeadAbsentSince >= grace.Milliseconds()
}

func parkProjectIfIdle(ctx context.Context, db *store.DB, project store.Project, now time.Time) (bool, error) {
	tasks, err := db.LiveTasks(ctx, project.ID)
	if err != nil {
		return false, err
	}
	if !projectReadyToPark(project, tasks, now, projectParkGrace) {
		return false, nil
	}
	return db.ParkProjectIfIdle(ctx, project, now, projectParkGrace)
}

func projectParkReason() string {
	return "Lead is missing and no Worker is active; its Watch is paused. Run `posse up` in the Project folder to resume it."
}
