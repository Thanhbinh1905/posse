package app

import (
	"context"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A second CLI must not display a half-torn-down landed Task while another
// process owns its unsaddle intent. Wait for its outcome or report progress.
func waitForActiveTeardowns(ctx context.Context, db *store.DB, projectID int64) error {
	deadline := time.NewTimer(10 * time.Second)
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
			if intent.Command != "unsaddle" {
				continue
			}
			task, err := db.TaskByID(ctx, projectID, intent.TaskID)
			if err != nil {
				return err
			}
			if task.State == store.StateLanded {
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
