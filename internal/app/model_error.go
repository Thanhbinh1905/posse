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
	kind   string
	marker string
}

const piErrorHelpLine = "If this looks like a pi bug, /bug sends a report to the developers."

var modelErrorPatterns = map[string][]modelErrorPattern{
	"pi": {
		{kind: "model_refused", marker: "this content was flagged for possible cybersecurity risk"},
		{kind: "model_stream_error", marker: "stream disconnected before completion: stream closed before response.completed"},
	},
}

var modelTerminalControl = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)

// classifyModelError recognizes Pi's rendered error result, not arbitrary
// transcript, User, or tool-output text containing a provider error string.
func classifyModelError(agent, output string) (kind, marker string, ok bool) {
	patterns := modelErrorPatterns[strings.ToLower(agent)]
	if len(patterns) == 0 {
		return "", "", false
	}
	lines := strings.Split(normalizeModelTerminalOutput(output), "\n")
	for i := 0; i+1 < len(lines); i++ {
		errorLine := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(strings.ToLower(errorLine), "error:") || strings.TrimSpace(lines[i+1]) != piErrorHelpLine {
			continue
		}
		message := strings.ToLower(strings.TrimSpace(errorLine[len("Error:"):]))
		if len(message) > 2048 || !piFooterOnly(lines[i+2:]) {
			continue
		}
		for _, pattern := range patterns {
			if strings.Contains(message, pattern.marker) {
				return pattern.kind, pattern.marker, true
			}
		}
	}
	return "", "", false
}

func piFooterOnly(lines []string) bool {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "/") || strings.Contains(line, "%/") {
			continue
		}
		border := line != ""
		for _, char := range line {
			if char != '─' && char != '━' && char != '═' && char != '-' {
				border = false
				break
			}
		}
		if !border {
			return false
		}
	}
	return true
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
	episode, observed, err := db.ObserveModelErrorEpisode(ctx, task.ID, task.Launches, pane.Agent, kind, fingerprint, task.IdleSince == 0, now)
	if err != nil {
		return nil, err
	}
	if !observed {
		if episode.Status == "active" && store.ModelErrorNudgeExpired(episode, time.UnixMilli(now)) {
			notice := modelErrorNotice(project, task, episode, kind, "A continue attempt was interrupted or its delivery is uncertain; Posse did not replay it", marker)
			created, err := db.InterruptExpiredModelErrorNudge(ctx, task.ID, now, notice)
			if err != nil || created == nil {
				return nil, err
			}
			return []store.Notice{*created}, nil
		}
		return nil, nil
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
		err = s.promptModelErrorContinue(ctx, db, task, pane, episode.Episode)
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
		} else if errors.As(err, &failure) && failure.Code == "model_error_task_not_working" {
			reason = "Automatic continue was withheld because the Task is no longer working"
		} else if ctx.Err() != nil {
			reason = "Automatic continue was interrupted before submission"
		}
		notice := modelErrorNotice(project, task, claimed, kind, reason, marker)
		created, finishErr := db.FinishModelErrorEpisode(ctx, task.ID, episode.Episode, status, reason, &notice, currentTime())
		if finishErr != nil {
			return nil, errors.Join(err, finishErr)
		}
		if created == nil {
			if ctx.Err() != nil {
				return nil, err
			}
			return nil, nil
		}
		return []store.Notice{*created}, nil
	}
	if err := db.CompleteModelErrorNudge(ctx, task.ID, episode.Episode, backoff.Milliseconds(), marker, currentTime()); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *Service) promptModelErrorContinue(ctx context.Context, db *store.DB, task store.Task, pane herdr.Pane, episode int) error {
	return s.safePromptWhenWithSubmissionGate(ctx, pane.PaneID, "continue", func(snapshot herdr.Snapshot) error {
		currentTask, err := db.TaskByID(ctx, task.ProjectID, task.ID)
		if err != nil {
			return err
		}
		if currentTask.State != store.StateWorking || currentTask.Launches != task.Launches || currentTask.PaneID != task.PaneID || currentTask.PaneLabel != task.PaneLabel || currentTask.HerdrWorkspaceID != task.HerdrWorkspaceID {
			return axi.Failure("model_error_task_not_working", "Task state, launch, or Rider ownership changed during model-error backoff", false)
		}
		current, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel)
		if !found || current.PaneID != pane.PaneID || current.Agent != pane.Agent || (current.AgentStatus != "idle" && current.AgentStatus != "done") {
			return axi.Failure("model_error_not_idle", "Rider is no longer at the end of an idle turn", false)
		}
		currentEpisode, err := db.TaskModelErrorEpisode(ctx, task.ID)
		if err != nil {
			return err
		}
		if currentEpisode.Episode != episode || currentEpisode.Launch != currentTask.Launches || currentEpisode.Status != "active" {
			return axi.Failure("model_error_task_not_working", "model-error episode is no longer eligible for an automatic continue", false)
		}
		_, err = db.OldestQueuedMessage(ctx, task.ID)
		if err == nil {
			return axi.Failure("model_error_message_queued", "a Lead instruction is queued for this Rider", false)
		}
		if !store.IsNotFound(err) {
			return err
		}
		return nil
	}, func(submit func() error) error {
		err := db.WithModelErrorPrompt(ctx, task, episode, submit)
		if errors.Is(err, store.ErrModelErrorPromptNotAllowed) {
			return axi.Failure("model_error_task_not_working", "Task or model-error episode changed before the continue could be submitted", false)
		}
		return err
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
