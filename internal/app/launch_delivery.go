package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

const launchDeliveryTimeout = 2 * time.Minute

// retryPendingLaunches runs on focus events and normal Project reconciliation
// (including lookout ticks). Only a finished ride intent can be retried here.
func (s *Service) retryPendingLaunches(ctx context.Context, db *store.DB, project store.Project, cfg config.Config, snapshot herdr.Snapshot) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.State != store.StateSpawning || task.PaneID == "" {
			continue
		}
		if _, err := db.IntentByTask(ctx, task.ID); err == nil {
			continue
		} else if !store.IsNotFound(err) {
			return err
		}
		launchPath := filepath.Join(home, "projects", project.Name, "tasks", taskIDString(task.Seq), "launch.md")
		if _, err := os.Stat(launchPath); err != nil {
			continue
		}
		pane, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel)
		if found && pane.Agent != "" && pane.AgentStatus != "blocked" && !pane.Focused && snapshot.FocusedPaneID != pane.PaneID {
			err := s.deliverLaunchPrompt(ctx, pane.PaneID, "Read "+launchPath+" and follow it.")
			if err == nil {
				if err := s.refreshWorkerDisplay(ctx, db, project, task.ID, taskKind(cfg, task)); err != nil {
					return err
				}
				if err := db.Transition(ctx, task.ID, store.StateSpawning, store.StateWorking, "cli", "Delivered the queued launch Brief"); err != nil && err != store.ErrStateRace {
					return err
				}
				continue
			}
			var failure *axi.Error
			if !errors.As(err, &failure) || failure.Code != "pane_focused" && failure.Code != "agent_blocked" {
				return err
			}
		}
		if time.Since(time.UnixMilli(task.CreatedAt)) >= launchDeliveryTimeout {
			if err := createNoticeOnce(ctx, db, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "spawn_waiting", Summary: task.Title + " is waiting for its launch Brief to be delivered", DataJSON: `{}`}); err != nil {
				return err
			}
		}
	}
	return nil
}
