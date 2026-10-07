package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/runtime"
	"github.com/thanhbinh1905/posse/internal/store"
)

const (
	modelStreamRetryLimit = 3
	modelStreamRetryDelay = 250 * time.Millisecond
)

type modelErrorPattern struct {
	kind        string
	marker      string
	maxTrailing int
}

var modelErrorPatterns = map[string][]modelErrorPattern{
	"pi": {
		{kind: "model_refused", marker: "this content was flagged for possible cybersecurity risk", maxTrailing: 2048},
		{kind: "model_stream_error", marker: "stream disconnected before completion: stream closed before response.completed", maxTrailing: 0},
	},
}

var modelTerminalControl = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)

// classifyModelError recognizes only known harness-specific errors at the end
// of an idle turn. It does not interpret prior transcript or User text as an
// active failure.
func classifyModelError(agent, output string) (kind, marker string, ok bool) {
	patterns := modelErrorPatterns[strings.ToLower(agent)]
	if len(patterns) == 0 {
		return "", "", false
	}
	output = normalizeModelTerminalOutput(output)
	lower := strings.ToLower(output)
	for _, pattern := range patterns {
		index := strings.LastIndex(lower, pattern.marker)
		if index < 0 {
			continue
		}
		trailing := strings.TrimSpace(lower[index+len(pattern.marker):])
		if len(trailing) > pattern.maxTrailing {
			continue
		}
		return pattern.kind, pattern.marker, true
	}
	return "", "", false
}

func normalizeModelTerminalOutput(output string) string {
	output = modelTerminalControl.ReplaceAllString(output, "")
	output = strings.ReplaceAll(output, "\r", "\n")
	return strings.TrimSpace(output)
}

func modelOutputFingerprint(output string) string {
	sum := sha256.Sum256([]byte(normalizeModelTerminalOutput(output)))
	return hex.EncodeToString(sum[:])
}

func (s *Service) handleModelErrorTurnEnd(ctx context.Context, db *store.DB, project store.Project, task store.Task, pane herdr.Pane) ([]store.Notice, error) {
	if task.State != store.StateWorking || pane.Agent == "" {
		return nil, nil
	}
	output, err := (runtime.SystemProgress{}).ReadPane(ctx, s.Herdr, pane.PaneID, 200)
	if err != nil {
		return nil, err
	}
	kind, marker, found := classifyModelError(pane.Agent, output)
	if !found {
		previous, err := db.TaskModelErrorEpisode(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		if previous.Launch == task.Launches && previous.Status == "active" {
			_, err := db.FinishModelErrorEpisode(ctx, task.ID, previous.Episode, "resolved", "Rider completed a turn without a recognized model error", nil, currentTime())
			return nil, err
		}
		return nil, nil
	}

	fingerprint := modelOutputFingerprint(output)
	now := currentTime()
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, task.ID, task.Launches, pane.Agent, kind, fingerprint, now)
	if err != nil || !observed {
		return nil, err
	}
	if kind == "model_refused" {
		notice := modelErrorNotice(project, task, episode, kind, "The provider refused the request; Posse did not retry the prompt", marker)
		created, err := db.FinishModelErrorEpisode(ctx, task.ID, episode.Episode, "refused", "Provider refusal recorded without an automatic retry", &notice, currentTime())
		if err != nil || created == nil {
			return nil, err
		}
		return []store.Notice{*created}, nil
	}

	if episode.Attempts >= modelStreamRetryLimit {
		notice := modelErrorNotice(project, task, episode, kind, fmt.Sprintf("Automatic continue nudges stopped after %d attempts", modelStreamRetryLimit), marker)
		created, err := db.FinishModelErrorEpisode(ctx, task.ID, episode.Episode, "exhausted", "Automatic stream-error recovery exhausted its bounded retry budget", &notice, currentTime())
		if err != nil || created == nil {
			return nil, err
		}
		return []store.Notice{*created}, nil
	}
	queued, err := db.OldestQueuedMessage(ctx, task.ID)
	if err != nil && !store.IsNotFound(err) {
		return nil, err
	}
	if err == nil {
		notice := modelErrorNotice(project, task, episode, kind, "Automatic retry was withheld because a Lead instruction is queued", marker)
		created, err := db.FinishModelErrorEpisode(ctx, task.ID, episode.Episode, "blocked", fmt.Sprintf("Automatic retry was withheld because queued Lead instruction #%d takes precedence", queued.ID), &notice, currentTime())
		if err != nil || created == nil {
			return nil, err
		}
		return []store.Notice{*created}, nil
	}

	attempt := episode.Attempts + 1
	backoff := modelStreamRetryDelay * time.Duration(1<<(attempt-1))
	claimed, ok, err := db.ClaimModelErrorNudge(ctx, task.ID, episode.Episode, episode.Fingerprint, modelStreamRetryLimit, currentTime(), backoff.Milliseconds())
	if err != nil || !ok {
		return nil, err
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = s.promptModelErrorContinue(ctx, db, task, pane)
	}
	if err != nil {
		status := "blocked"
		reason := "Automatic continue was withheld because the Rider pane was not safe to prompt"
		var failure *axi.Error
		if errors.As(err, &failure) && failure.Code == "pane_focused" {
			reason = "Automatic continue was withheld because the Rider pane is focused"
		} else if errors.As(err, &failure) && failure.Code == "agent_blocked" {
			reason = "Automatic continue was withheld because the Rider is blocked"
		} else if errors.As(err, &failure) && failure.Code == "model_error_message_queued" {
			reason = "Automatic continue was withheld because a Lead instruction was queued"
		} else if errors.As(err, &failure) && failure.Code == "model_error_not_idle" {
			reason = "Automatic continue was withheld because the Rider was no longer idle"
		} else if ctx.Err() != nil {
			reason = "Automatic continue was interrupted before submission"
		}
		notice := modelErrorNotice(project, task, claimed, kind, reason, marker)
		created, finishErr := db.FinishModelErrorEpisode(ctx, task.ID, episode.Episode, status, reason, &notice, currentTime())
		if finishErr != nil {
			return nil, errors.Join(err, finishErr)
		}
		if created == nil {
			return nil, err
		}
		return []store.Notice{*created}, nil
	}
	if err := db.CompleteModelErrorNudge(ctx, task.ID, episode.Episode, backoff.Milliseconds(), marker, currentTime()); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Service) promptModelErrorContinue(ctx context.Context, db *store.DB, task store.Task, pane herdr.Pane) error {
	return s.safePromptWhen(ctx, pane.PaneID, "continue", func(snapshot herdr.Snapshot) error {
		current, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel)
		if !found || current.PaneID != pane.PaneID || current.Agent != pane.Agent || (current.AgentStatus != "idle" && current.AgentStatus != "done") {
			return axi.Failure("model_error_not_idle", "Rider is no longer at the end of an idle turn", false)
		}
		_, err := db.OldestQueuedMessage(ctx, task.ID)
		if err == nil {
			return axi.Failure("model_error_message_queued", "a Lead instruction is queued for this Rider", false)
		}
		if !store.IsNotFound(err) {
			return err
		}
		return nil
	})
}

func modelErrorNotice(project store.Project, task store.Task, episode store.ModelErrorEpisode, kind, reason, marker string) store.Notice {
	maxAttempts := modelStreamRetryLimit
	attempts := fmt.Sprintf("%d/%d continue nudges", episode.Attempts, maxAttempts)
	if kind == "model_refused" {
		maxAttempts = 0
		attempts = "no automatic retries"
	}
	return store.Notice{
		ProjectID: project.ID,
		TaskID:    task.ID,
		Kind:      kind,
		Summary:   fmt.Sprintf("%s: %s (episode %d, %s)", task.Title, reason, episode.Episode, attempts),
		DataJSON:  marshalJSON(map[string]any{"episode": episode.Episode, "launch": episode.Launch, "agent": episode.Agent, "kind": kind, "attempts": episode.Attempts, "max_attempts": maxAttempts, "marker": marker}),
	}
}
