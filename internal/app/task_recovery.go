package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A retryable Rider failure is persisted, not returned through unrelated CLI
// operations. The project generation stays pending until every Rider settles.
var errRecoveryDeferred = errors.New("rider recovery is waiting for its retry backoff")

func recoveryBackoff(initial time.Duration, attempts int) time.Duration {
	delay := min(initial, time.Minute)
	for i := 1; i < attempts && delay < time.Minute; i++ {
		delay = min(delay*2, time.Minute)
	}
	return delay
}

// recoverTask returns whether this generation is settled and whether it was
// recovered. Project claims serialize recovery passes; the Task claim survives
// crashes and charges the budget before entering the launch path.
func (s *Service) recoverTask(ctx context.Context, db *store.DB, home string, project store.Project, cfg config.Config, task store.Task, generation string, snapshot herdr.Snapshot) (settled, recovered bool, err error) {
	state, err := db.TaskRecovery(ctx, task.ID)
	if err != nil {
		return false, false, err
	}
	if state.Status == "exhausted" {
		return true, false, nil
	}
	if state.Status == "recovered" {
		pane, found := findTaskPane(snapshot.Panes, task)
		live := found && pane.Agent != "" && pane.AgentStatus != "exited" && pane.AgentStatus != "stopped" && task.AgentServerStartedAt == snapshot.ServerStartedAt
		// Group recovery can change its own Lead/workspace ids. A Rider that
		// already recovered and is still live must not follow that mismatch.
		if state.Generation == generation || live {
			return true, true, nil
		}
	}
	if state.OwnerPID != 0 && processAlive(state.OwnerPID) {
		return false, false, nil
	}
	if state.Attempts >= cfg.Defaults.RecoveryAttempts && state.Status != "recovered" {
		if err := db.FinishTaskRecovery(ctx, task, state.OwnerPID, cfg.Defaults.RecoveryAttempts, false, "recovery process exited before recording its outcome: "+state.LastError, 0); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	now := time.Now()
	attempts := state.Attempts + 1
	if state.Status == "recovered" {
		attempts = 1
	}
	claimed, err := db.ClaimTaskRecovery(ctx, task.ID, generation, state.OwnerPID, os.Getpid(), cfg.Defaults.RecoveryAttempts, now.UnixMilli(), now.Add(recoveryBackoff(duration(cfg.Defaults.RecoveryBackoff), attempts)).UnixMilli())
	if err != nil || !claimed {
		return false, false, err
	}
	_, launchErr := s.relaunchTaskAttempt(ctx, db, home, project, cfg, task, "", true)
	cause := ""
	if launchErr != nil {
		cause = launchErr.Error()
	}
	// Even a canceled launch must release its claim and persist its outcome.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := db.FinishTaskRecovery(finishCtx, task, os.Getpid(), cfg.Defaults.RecoveryAttempts, launchErr == nil, cause, time.Now().Add(recoveryBackoff(duration(cfg.Defaults.RecoveryBackoff), attempts)).UnixMilli()); err != nil {
		return false, false, err
	}
	if launchErr == nil {
		current, err := db.TaskByID(ctx, project.ID, task.ID)
		if err != nil {
			return false, false, err
		}
		return true, current.State != store.StateFailed, nil
	}
	return attempts >= cfg.Defaults.RecoveryAttempts, false, nil
}
