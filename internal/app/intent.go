package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) startTaskIntent(ctx context.Context, db *store.DB, projectID, taskID int64, command string) (store.Intent, error) {
	return s.startTaskIntentWithPayload(ctx, db, projectID, taskID, command, `{}`)
}

func (s *Service) startTaskIntentWithPayload(ctx context.Context, db *store.DB, projectID, taskID int64, command, payload string) (store.Intent, error) {
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
	if err := db.StartIntent(ctx, projectID, taskID, command, "started", payload, processID); err != nil {
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

func (s *Service) beginIntentStep(ctx context.Context, db *store.DB, intent store.Intent, step string) error {
	return db.UpdateIntentStep(ctx, intent.ID, os.Getpid(), "in_progress:"+step)
}

func (s *Service) runIntentStep(ctx context.Context, db *store.DB, intent store.Intent, step string, action func() error) error {
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
