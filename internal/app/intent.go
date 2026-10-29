package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) startTaskIntent(ctx context.Context, db *store.DB, projectID, taskID int64, command string) (store.Intent, error) {
	return s.startTaskIntentWithPayload(ctx, db, projectID, taskID, command, `{}`)
}

func (s *Service) startTaskIntentWithPayload(ctx context.Context, db *store.DB, projectID, taskID int64, command, payload string) (store.Intent, error) {
	return s.startTaskIntentWithInitialStep(ctx, db, projectID, taskID, command, payload, "started")
}

func (s *Service) startTaskIntentAtStep(ctx context.Context, db *store.DB, projectID, taskID int64, command, step string) (store.Intent, error) {
	return s.startTaskIntentWithInitialStep(ctx, db, projectID, taskID, command, `{}`, step)
}

func (s *Service) startTaskIntentWithInitialStep(ctx context.Context, db *store.DB, projectID, taskID int64, command, payload, step string) (store.Intent, error) {
	var intent store.Intent
	err := withSandboxStateLock(ctx, db, func() error {
		var err error
		intent, err = s.startTaskIntentWithPayloadLockedAtStep(ctx, db, projectID, taskID, command, payload, step)
		return err
	})
	return intent, err
}

func (s *Service) startTaskIntentWithPayloadLockedAtStep(ctx context.Context, db *store.DB, projectID, taskID int64, command, payload, initialStep string) (store.Intent, error) {
	processID := os.Getpid()
	intent, err := db.IntentByTask(ctx, taskID)
	if err == nil {
		if intent.ProcessID == processID && intent.Command == command {
			return intent, nil
		}
		if intent.Command != command || intentProcessAlive(intent) {
			return store.Intent{}, fmt.Errorf("task already has an active %s intent at %s", intent.Command, intent.Step)
		}
		claimed, claimErr := db.ClaimIntent(ctx, intent.ID, intent.ProcessID, processID, "resuming:"+intent.Step)
		if claimErr != nil {
			return store.Intent{}, claimErr
		}
		if !claimed {
			return store.Intent{}, store.ErrIntentOwned
		}
		intent.ProcessID = processID
		return intent, nil
	}
	if !store.IsNotFound(err) {
		return store.Intent{}, err
	}
	if err := db.StartIntent(ctx, projectID, taskID, command, initialStep, payload, processID); err != nil {
		return store.Intent{}, err
	}
	return db.IntentByTask(ctx, taskID)
}

func processAlive(processID int) bool {
	if processID < 1 {
		return false
	}
	err := syscall.Kill(processID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func intentProcessAlive(intent store.Intent) bool {
	if !processAlive(intent.ProcessID) {
		return false
	}
	if intent.ProcessBootID == "" || intent.ProcessStartTime == "" {
		return true
	}
	bootID, startTime, err := store.ProcessIdentityForPID(intent.ProcessID)
	return err == nil && bootID == intent.ProcessBootID && startTime == intent.ProcessStartTime
}

func (s *Service) holdTaskTeardown(ctx context.Context, db *store.DB, taskID int64) (func(), error) {
	key := fmt.Sprintf("%s:%d", db.Path, taskID)
	for {
		s.teardownMu.Lock()
		held, busy := s.teardowns[key]
		if !busy {
			if s.teardowns == nil {
				s.teardowns = map[string]chan struct{}{}
			}
			released := make(chan struct{})
			s.teardowns[key] = released
			s.teardownMu.Unlock()
			return func() {
				s.teardownMu.Lock()
				delete(s.teardowns, key)
				s.teardownMu.Unlock()
				close(released)
			}, nil
		}
		s.teardownMu.Unlock()
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *Service) beginIntentStep(ctx context.Context, db *store.DB, intent store.Intent, step string) error {
	return db.UpdateIntentStep(ctx, intent.ID, os.Getpid(), "in_progress:"+step)
}

func teardownStepIndex(step string) int {
	steps := []string{"worker.stop", "approval.record", "discard.capture", "process.stop", "panes.close", "sandbox.stop", "orphan.snapshot", "report.attachments", "scratch.remove", "sandbox.release", "branch.remove", "task.torn_down"}
	for index, candidate := range steps {
		if candidate == step {
			return index
		}
	}
	return -1
}

func (s *Service) runIntentStep(ctx context.Context, db *store.DB, intent store.Intent, step string, action func() error) error {
	if intent.Command == "teardown" && intent.TaskID > 0 {
		current, err := db.IntentByTask(ctx, intent.TaskID)
		if err != nil {
			return err
		}
		priorStep := strings.TrimPrefix(current.Step, "resuming:")
		priorDone := strings.HasPrefix(priorStep, "done:")
		priorName := strings.TrimPrefix(strings.TrimPrefix(priorStep, "done:"), "in_progress:")
		if priorDone && priorName == step {
			return nil
		}
		if teardownStepIndex(priorName) > teardownStepIndex(step) {
			// Older A01 Teardown stopped processes before verifying pane removal.
			// Recheck that safety boundary when resuming such an intent.
			if step != "panes.close" || priorName != "sandbox.stop" {
				return nil
			}
		}
	}
	if err := s.beginIntentStep(ctx, db, intent, step); err != nil {
		return err
	}
	crashIntentAt(intent.Command, "before", step)
	if err := action(); err != nil {
		return err
	}
	return s.completeIntentStep(ctx, db, intent, step)
}

func (s *Service) completeIntentStep(ctx context.Context, db *store.DB, intent store.Intent, step string) error {
	if err := db.UpdateIntentStep(ctx, intent.ID, os.Getpid(), "done:"+step); err != nil {
		return err
	}
	crashIntentAt(intent.Command, "after", step)
	return nil
}

func crashIntentAt(command, phase, step string) {
	// Isolated E2E can hold a live command exactly between intent steps
	// while its Herdr group closes. Never enable the pause in a User home.
	if os.Getenv("POSSE_TEST_ROOT") != "" && os.Getenv("POSSE_INTENT_PAUSE_AT") == command+":"+phase+":"+step {
		if marker := os.Getenv("POSSE_INTENT_PAUSE_FILE"); marker != "" {
			_ = os.WriteFile(marker, []byte(step), 0o600)
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
				if _, err := os.Stat(marker + ".continue"); err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	crashAt := os.Getenv("POSSE_INTENT_CRASH_AT")
	if crashAt == command+":"+phase+":"+step || (phase == "after" && (crashAt == step || crashAt == command+":"+step)) {
		os.Exit(86)
	}
}

// finishSettledTeardownIntent completes a Teardown intent whose Task already
// reached torn-down. Only idempotent bookkeeping follows that step, so another
// Teardown may finish it even while the recorded owner process stays alive.
func (s *Service) finishSettledTeardownIntent(ctx context.Context, db *store.DB, projectID, taskID int64) (bool, error) {
	intent, err := db.IntentByTask(ctx, taskID)
	if store.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if intent.Command != "teardown" || strings.TrimPrefix(intent.Step, "resuming:") != "done:task.torn_down" {
		return false, nil
	}
	task, err := db.TaskByID(ctx, projectID, taskID)
	if err != nil || task.State != store.StateTornDown {
		return false, err
	}
	if err := s.regenerateProjects(ctx, db); err != nil {
		return false, err
	}
	if err := db.FinishIntent(ctx, intent.ID, intent.ProcessID); err != nil && !errors.Is(err, store.ErrIntentOwned) {
		return false, err
	}
	return true, nil
}

func (s *Service) finishTaskIntent(ctx context.Context, db *store.DB, taskID int64) error {
	intent, err := db.IntentByTask(ctx, taskID)
	if store.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return db.FinishIntent(ctx, intent.ID, os.Getpid())
}
