package app

import (
	"context"
	"errors"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

var teardownWaitTimeout = 10 * time.Second

func isTeardownInProgress(err error) bool {
	var failure *axi.Error
	return errors.As(err, &failure) && failure.Code == "teardown_in_progress"
}

// A second CLI must not display a half-torn-down merged Task while another
// process owns its teardown intent. Wait for its outcome or report progress.
func waitForActiveTeardowns(ctx context.Context, db *store.DB, projectID int64) error {
	return waitForTeardowns(ctx, db, projectID, func(int64) bool { return true })
}

func waitForTaskTeardown(ctx context.Context, db *store.DB, projectID, taskID int64) error {
	return waitForTeardowns(ctx, db, projectID, func(id int64) bool { return id == taskID })
}

func waitForTeardowns(ctx context.Context, db *store.DB, projectID int64, covers func(taskID int64) bool) error {
	deadline := time.NewTimer(teardownWaitTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		intents, err := db.Intents(ctx, projectID)
		if err != nil {
			return err
		}
		active := false
		for _, intent := range intents {
			if intent.Command != "teardown" || !covers(intent.TaskID) {
				continue
			}
			task, err := db.TaskByID(ctx, projectID, intent.TaskID)
			if err != nil {
				return err
			}
			if task.State == store.StateMerged {
				active = true
				break
			}
		}
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return axi.Failure("teardown_in_progress", "Task Teardown is in progress", true, "Retry after the active Teardown completes")
		case <-deadline.C:
			return axi.Failure("teardown_in_progress", "Task Teardown is in progress", true, "Retry after the active Teardown completes")
		case <-ticker.C:
		}
	}
}
