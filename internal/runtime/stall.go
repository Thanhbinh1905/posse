package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func EvaluateStalls(ctx context.Context, db *store.DB, adapter herdr.Adapter, projectID int64, stallAfter time.Duration, now time.Time, progress ProgressSource) ([]store.Notice, error) {
	snapshot, err := adapter.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return evaluateStallsSnapshot(ctx, db, adapter, projectID, snapshot, stallAfter, now, progress)
}

func evaluateStallsSnapshot(ctx context.Context, db *store.DB, adapter herdr.Adapter, projectID int64, snapshot herdr.Snapshot, stallAfter time.Duration, now time.Time, progress ProgressSource) ([]store.Notice, error) {
	if progress == nil {
		progress = SystemProgress{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	tasks, err := db.StallTasks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var notices []store.Notice
	var failures []error
	for _, task := range tasks {
		if err := reconcileWorkDeferred(ctx); err != nil {
			return notices, errors.Join(append(failures, err)...)
		}
		pane, found := findPane(snapshot.Panes, task.PaneID, task.PaneLabel)
		if !found || pane.Agent == "" || (task.State == store.StateWorking && pane.AgentStatus != "working") {
			if task.State == store.StateWorking {
				if err := db.ResetProgress(ctx, task.ID); err != nil {
					failures = append(failures, err)
				}
			}
			continue
		}
		output, err := progress.ReadPane(ctx, adapter, pane.PaneID, 200)
		if err != nil {
			if missingPaneReadError(err) {
				if task.State == store.StateWorking {
					if err := db.ResetProgress(ctx, task.ID); err != nil {
						failures = append(failures, err)
					}
				}
				continue
			}
			failures = append(failures, fmt.Errorf("read Rider pane %s: %w", taskID(task.Seq), err))
			continue
		}
		worktree, err := progress.WorktreeFingerprint(ctx, task.WorktreePath)
		if err != nil {
			failures = append(failures, fmt.Errorf("read Rider worktree %s: %w", taskID(task.Seq), err))
			continue
		}
		outputHash := digest(output)
		worktreeHash := digest(worktree)
		changed := task.LastProgressAt == 0 || task.LastOutputHash != outputHash || task.LastWorktreeHash != worktreeHash
		if task.State == store.StateStalled {
			if changed {
				if err := db.Transition(ctx, task.ID, store.StateStalled, store.StateWorking, "cli", "Rider output or worktree changed"); err != nil {
					if !errors.Is(err, store.ErrStateRace) {
						failures = append(failures, err)
					}
				}
			}
			continue
		}
		if changed {
			if err := db.UpdateProgress(ctx, task.ID, outputHash, worktreeHash, now.UnixMilli()); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if now.Sub(time.UnixMilli(task.LastProgressAt)) < stallAfter {
			continue
		}
		if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateStalled, "cli", "Rider output and worktree unchanged for "+stallAfter.String()); err != nil {
			if errors.Is(err, store.ErrStateRace) {
				continue
			}
			failures = append(failures, err)
			continue
		}
		notice, err := createNotice(ctx, db, task.ProjectID, task.ID, "stalled", task.Title+" appears stalled", now)
		if err != nil {
			failures = append(failures, err)
		} else {
			notices = append(notices, notice)
		}
	}
	return notices, errors.Join(failures...)
}

func missingPaneReadError(err error) bool {
	var apiError *herdr.Error
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.Code {
	case "pane_not_found", "tab_not_found", "workspace_not_found", "not_found":
		return true
	default:
		return false
	}
}

func (SystemProgress) ReadPane(ctx context.Context, adapter herdr.Adapter, paneID string, lines int) (string, error) {
	raw, err := adapter.Call(ctx, "pane.read", map[string]any{"pane_id": paneID, "source": "recent_unwrapped", "lines": lines})
	if err != nil {
		return "", err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	for _, key := range []string{"text", "output", "content", "read"} {
		if text, ok := value[key].(string); ok {
			return text, nil
		}
	}
	if result, ok := value["result"].(map[string]any); ok {
		for _, key := range []string{"text", "output", "content", "read"} {
			if text, ok := result[key].(string); ok {
				return text, nil
			}
		}
	}
	if read, ok := value["read"].(map[string]any); ok {
		if text, ok := read["text"].(string); ok {
			return text, nil
		}
	}
	return "", fmt.Errorf("pane.read result did not include text")
}

// WorktreeFingerprint summarizes HEAD and status of a Mount. A workspace Mount
// is a plain folder, so its fingerprint covers every member worktree inside it.
func (SystemProgress) WorktreeFingerprint(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("worktree path is empty")
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return repositoryFingerprint(ctx, path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", err
	}
	var fingerprint strings.Builder
	for _, entry := range entries {
		member := filepath.Join(path, entry.Name())
		if _, err := os.Stat(filepath.Join(member, ".git")); !entry.IsDir() || err != nil {
			continue
		}
		part, err := repositoryFingerprint(ctx, member)
		if err != nil {
			return "", err
		}
		fingerprint.WriteString(entry.Name() + "\n" + part)
	}
	if fingerprint.Len() == 0 {
		return "", fmt.Errorf("%s holds no worktree", path)
	}
	return fingerprint.String(), nil
}

func repositoryFingerprint(ctx context.Context, path string) (string, error) {
	head, err := execgroup.CommandContext(ctx, "git", "-C", path, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, strings.TrimSpace(string(head)))
	}
	status, err := execgroup.CommandContext(ctx, "git", "-C", path, "status", "--porcelain").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(string(status)))
	}
	return strings.TrimSpace(string(head)) + "\n" + string(status), nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
