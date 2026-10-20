package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thanhbinh1905/posse/internal/execution"
	"github.com/thanhbinh1905/posse/internal/store"
)

// A held repository Sandbox stays protected across Posse and Herdr restarts.
// Missing checkouts are reported once rather than recreated over lost work.
func ensureHeldSandboxLocks(ctx context.Context, db *store.DB, project store.Project) error {
	return withSandboxStateLock(ctx, db, func() error {
		if err := execution.NewManager(db).EnsureSandboxLeases(ctx); err != nil {
			return resourceLeaseError(err)
		}
		sandboxes, err := db.Sandboxes(ctx, project.ID)
		if err != nil {
			return err
		}
		for _, sandbox := range sandboxes {
			if sandbox.State == "releasing" && sandbox.TaskID != 0 {
				task, err := db.TaskByID(ctx, project.ID, sandbox.TaskID)
				if err != nil {
					return err
				}
				if err := finishReleasingSandboxLocked(ctx, db, project, sandbox, task); err != nil {
					return err
				}
				continue
			}
			if project.IsWorkspace() || sandbox.State != "held" || sandbox.TaskID == 0 {
				continue
			}
			task, err := db.TaskByID(ctx, project.ID, sandbox.TaskID)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(sandbox.Path, ".git")); err != nil {
				if !os.IsNotExist(err) {
					return err
				}
				intent, intentErr := db.IntentByTask(ctx, task.ID)
				if intentErr != nil && !store.IsNotFound(intentErr) {
					return intentErr
				}
				if task.State == store.StateSpawning && intentErr == nil && intent.Command == "start" {
					// The start intent owns the gap between the durable Sandbox
					// claim and Git materializing its worktree metadata. It will
					// either finish the checkout or recover the interrupted start.
					continue
				}
				found, err := hasOpenSandboxMissingNotice(ctx, db, project.ID, task.ID)
				if err != nil {
					return err
				}
				if !found {
					_, err = db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "sandbox_missing", Summary: fmt.Sprintf("sandbox-%d disappeared while held by t%d; inspect the missing checkout before recovery", sandbox.Number, task.Seq), DataJSON: `{}`})
				}
				if err != nil {
					return err
				}
				continue
			}
			if err := resolveRecoveredSandboxMissingNotice(ctx, db, project, task); err != nil {
				return err
			}
			if err := lockSandbox(ctx, project.Root, sandbox.Path, task.Seq); err != nil {
				return err
			}
		}
		return nil
	})
}

// lockClaimedSandbox replaces only a stale lock left by another Posse Task.
// Ownership must already be assigned to this Task in the Sandbox table.
func lockClaimedSandbox(ctx context.Context, db *store.DB, project store.Project, sandbox store.Sandbox, task store.Task) error {
	claim, err := db.SandboxByTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if claim.ID != sandbox.ID || claim.Path != sandbox.Path || claim.State != "held" || claim.TaskID != task.ID {
		return store.ErrStateRace
	}
	return lockAvailableSandbox(ctx, db, project, sandbox, task)
}

// Call under withSandboxStateLock before claiming an idle checkout, or after
// claiming a new checkout. Git protects the checkout before it becomes held.
func lockAvailableSandbox(ctx context.Context, db *store.DB, project store.Project, sandbox store.Sandbox, task store.Task) error {
	current, err := sandboxLockReason(ctx, project.Root, sandbox.Path)
	if err != nil {
		return err
	}
	wanted := fmt.Sprintf("posse: held by t%d", task.Seq)
	if current != "" && current != wanted {
		old, ok := posseLockTaskSeq(current)
		if !ok {
			return fmt.Errorf("refuse to replace foreign worktree lock %q on %s", current, sandbox.Path)
		}
		former, lookupErr := db.Task(ctx, project.ID, fmt.Sprintf("t%d", old))
		if lookupErr != nil && lookupErr != store.ErrNotFound {
			return lookupErr
		}
		if lookupErr == nil && former.SandboxID == sandbox.ID {
			return store.ErrStateRace
		}
		if err := unlockSandbox(ctx, project.Root, sandbox.Path); err != nil {
			return err
		}
	}
	return lockSandbox(ctx, project.Root, sandbox.Path, task.Seq)
}

func hasOpenSandboxMissingNotice(ctx context.Context, db *store.DB, projectID, taskID int64) (bool, error) {
	notices, err := db.Notices(ctx, projectID, true)
	if err != nil {
		return false, err
	}
	for _, notice := range notices {
		if notice.TaskID == taskID && notice.Kind == "sandbox_missing" {
			return true, nil
		}
	}
	return false, nil
}

// resolveRecoveredSandboxMissingNotice retires a missing-Sandbox Notice once
// reconciliation verifies that the held checkout exists again. Without this,
// a stale Notice from an interrupted creation can suppress a later real loss.
func resolveRecoveredSandboxMissingNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task) error {
	notices, err := db.Notices(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, notice := range notices {
		if notice.TaskID != task.ID || notice.Kind != "sandbox_missing" {
			continue
		}
		if _, err := db.AckNotices(ctx, project.ID, []string{strconv.FormatInt(notice.ID, 10)}); err != nil {
			return err
		}
	}
	return nil
}

func posseLockTaskSeq(reason string) (int, bool) {
	const prefix = "posse: held by t"
	if !strings.HasPrefix(reason, prefix) {
		return 0, false
	}
	seq, err := strconv.Atoi(strings.TrimPrefix(reason, prefix))
	return seq, err == nil && seq > 0 && reason == fmt.Sprintf("%s%d", prefix, seq)
}
