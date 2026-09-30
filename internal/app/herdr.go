package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
)

func (s *Service) herdrCall(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if s.Herdr == nil {
		return nil, axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true, "Run `posse doctor` to inspect setup")
	}
	result, err := s.Herdr.Call(ctx, method, params)
	if err != nil {
		return nil, fmt.Errorf("call Herdr method %s: %w", method, herdrError(err))
	}
	return result, nil
}

func (s *Service) snapshot(ctx context.Context) (herdr.Snapshot, error) {
	if s.Herdr == nil {
		return herdr.Snapshot{}, axi.Failure("herdr_unavailable", "Herdr adapter is not configured", true)
	}
	if err := s.Herdr.CheckProtocol(ctx); err != nil {
		return herdr.Snapshot{}, herdrError(err)
	}
	snapshot, err := s.Herdr.Snapshot(ctx)
	if err != nil {
		return herdr.Snapshot{}, fmt.Errorf("call Herdr method session.snapshot: %w", herdrError(err))
	}
	return snapshot, nil
}

func (s *Service) safePrompt(ctx context.Context, paneID, text string) error {
	return s.safePromptWhen(ctx, paneID, text, nil)
}

func (s *Service) safePromptWhen(ctx context.Context, paneID, text string, allowed func(herdr.Snapshot) error) error {
	return s.safePromptWhenBefore(ctx, paneID, text, allowed, nil)
}

func (s *Service) safePromptWhenBefore(ctx context.Context, paneID, text string, allowed func(herdr.Snapshot) error, beforePrompt func() error) error {
	return s.promptBefore(ctx, paneID, map[string]any{"target": paneID, "text": text}, allowed, beforePrompt)
}

func (s *Service) prompt(ctx context.Context, paneID string, params map[string]any, allowed func(herdr.Snapshot) error) error {
	return s.promptBefore(ctx, paneID, params, allowed, nil)
}

func (s *Service) promptBefore(ctx context.Context, paneID string, params map[string]any, allowed func(herdr.Snapshot) error, beforePrompt func() error) error {
	return s.typeIntoBefore(ctx, paneID, "agent.prompt", params, allowed, beforePrompt)
}

// typeInto sends input to an agent pane unless the User is in it or the agent
// waits at a dialog.
func (s *Service) typeInto(ctx context.Context, paneID, method string, params map[string]any, allowed func(herdr.Snapshot) error) error {
	return s.typeIntoBefore(ctx, paneID, method, params, allowed, nil)
}

func (s *Service) typeIntoBefore(ctx context.Context, paneID, method string, params map[string]any, allowed func(herdr.Snapshot) error, beforeSend func() error) error {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID != paneID {
			continue
		}
		if pane.Focused || snapshot.FocusedPaneID == paneID {
			return axi.Failure("pane_focused", "refusing to type into a focused pane", false, "Unfocus the pane, then retry")
		}
		if pane.AgentStatus == "blocked" {
			return axi.Failure("agent_blocked", "refusing to type into a blocked agent", false, "Inspect the Rider dialog before retrying")
		}
	}
	if allowed != nil {
		if err := allowed(snapshot); err != nil {
			return err
		}
	}
	if beforeSend != nil {
		if err := beforeSend(); err != nil {
			return err
		}
	}
	_, err = s.herdrCall(ctx, method, params)
	return err
}

var (
	agentStartBusyWait   = 10 * time.Second
	agentStartRetryDelay = 250 * time.Millisecond
)

// startAgent starts an agent in a pane that may have just opened. Herdr queues
// a launch before the pane's shell starts, but rejects it with agent_pane_busy
// while the shell is still running its startup commands, so the start is
// retried until the shell reaches its prompt.
func (s *Service) startAgent(ctx context.Context, params map[string]any) (json.RawMessage, error) {
	deadline := time.Now().Add(agentStartBusyWait)
	for {
		started, err := s.herdrCall(ctx, "agent.start", params)
		var failure *axi.Error
		if err == nil || !errors.As(err, &failure) || failure.Code != "agent_pane_busy" || time.Now().After(deadline) {
			return started, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(agentStartRetryDelay):
		}
	}
}

const (
	launchPromptAttempts     = 4
	launchPromptActivityWait = 10 * time.Second
)

var launchPromptRetryDelay = time.Second

// deliverLaunchPrompt submits the first prompt to a freshly started agent and
// confirms that the agent started working on it. An agent TUI can report
// interactive readiness before it reads input, and Codex then silently drops
// the submission, so the prompt is resent until Herdr observes activity.
func (s *Service) deliverLaunchPrompt(ctx context.Context, paneID, text string) error {
	return s.confirmPrompt(ctx, paneID, text, func(ctx context.Context, method string, params map[string]any) error {
		return s.typeInto(ctx, paneID, method, params, nil)
	})
}

// deliverLeadPrompt is deliverLaunchPrompt for a Lead started in the pane that
// ran `posse up`. That pane is focused by design, so the focused-pane guard
// that protects Worker panes from typing over the User does not apply.
func (s *Service) deliverLeadPrompt(ctx context.Context, paneID, text string) error {
	return s.confirmPrompt(ctx, paneID, text, func(ctx context.Context, method string, params map[string]any) error {
		_, err := s.herdrCall(ctx, method, params)
		return err
	})
}

// confirmPrompt types text into the agent and submits it until Herdr observes
// the agent working. When a submission stalls with the text still on screen,
// only its Enter was lost, as with Pi, so Enter is pressed instead of retyping
// the text next to the unsubmitted copy.
func (s *Service) confirmPrompt(ctx context.Context, paneID, text string, send func(ctx context.Context, method string, params map[string]any) error) error {
	activity := map[string]any{"until": []string{"working", "blocked"}, "timeout_ms": launchPromptActivityWait.Milliseconds()}
	var lastErr error
	pressedEnter := false
	for attempt := 1; attempt <= launchPromptAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(launchPromptRetryDelay):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, launchPromptActivityWait+5*time.Second)
		if attempt > 1 && !pressedEnter && s.promptOnScreen(attemptCtx, paneID, text) {
			pressedEnter = true
			lastErr = send(attemptCtx, "agent.send_keys", map[string]any{"target": paneID, "keys": []string{"enter"}})
			if lastErr == nil {
				_, lastErr = s.herdrCall(attemptCtx, "agent.wait", map[string]any{"target": paneID, "until": activity["until"], "timeout_ms": activity["timeout_ms"]})
			}
		} else {
			pressedEnter = false
			lastErr = send(attemptCtx, "agent.prompt", map[string]any{"target": paneID, "text": text, "wait": activity})
		}
		cancel()
		if lastErr == nil {
			return nil
		}
		if !isPromptStall(lastErr) {
			return lastErr
		}
		if s.agentActive(ctx, paneID) {
			return nil
		}
	}
	return axi.Failure("prompt_not_delivered", fmt.Sprintf("the agent did not start working after %d prompt attempts: %v", launchPromptAttempts, lastErr), true, "Inspect the agent pane in Herdr, then retry")
}

// promptOnScreen reports whether the pane shows text, ignoring whitespace so a
// prompt wrapped by the agent's input box still matches. A stalled prompt was
// never submitted, so a copy on screen is the one left in the input.
func (s *Service) promptOnScreen(ctx context.Context, paneID, text string) bool {
	raw, err := s.herdrCall(ctx, "pane.read", map[string]any{"pane_id": paneID, "source": "visible"})
	if err != nil {
		return false
	}
	var screen struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if json.Unmarshal(raw, &screen) != nil {
		return false
	}
	compact := func(value string) string { return strings.Join(strings.Fields(value), "") }
	want := compact(text)
	return want != "" && strings.Contains(compact(screen.Read.Text), want)
}

func isPromptStall(err error) bool {
	var failure *axi.Error
	return errors.As(err, &failure) && (failure.Code == "agent_prompt_stalled" || failure.Code == "timeout")
}

func (s *Service) agentActive(ctx context.Context, paneID string) bool {
	raw, err := s.herdrCall(ctx, "agent.get", map[string]any{"target": paneID})
	if err != nil {
		return false
	}
	var response struct {
		Agent struct {
			AgentStatus string `json:"agent_status"`
		} `json:"agent"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return false
	}
	return response.Agent.AgentStatus == "working" || response.Agent.AgentStatus == "blocked"
}

func findAppPane(panes []herdr.Pane, paneID, label string) (herdr.Pane, bool) {
	return herdr.FindPane(panes, paneID, label)
}

var (
	agentStopGrace = 5 * time.Second
	agentKillGrace = 2 * time.Second
)

// stopPaneAgent ends the agent in the pane and waits until the pane's shell is
// back in the foreground. Herdr has no call that stops an agent, so posse
// signals the pane's foreground process group: SIGTERM, then SIGKILL.
func (s *Service) stopPaneAgent(ctx context.Context, paneID string) error {
	group, err := s.paneAgentGroup(ctx, paneID)
	if err != nil || group == 0 {
		return err
	}
	for _, step := range []struct {
		signal syscall.Signal
		grace  time.Duration
	}{{syscall.SIGTERM, agentStopGrace}, {syscall.SIGKILL, agentKillGrace}} {
		if err := syscall.Kill(-group, step.signal); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("signal agent process group %d: %w", group, err)
		}
		deadline := time.Now().Add(step.grace)
		for time.Now().Before(deadline) {
			current, err := s.paneAgentGroup(ctx, paneID)
			if err != nil {
				return err
			}
			if current != group {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return axi.Failure("agent_stop_failed", fmt.Sprintf("the agent in pane %s did not exit", paneID), true, "Close the pane, then retry")
}

// paneAgentGroup returns the pane's foreground process group when it is not
// the pane's shell, and 0 when the shell is in the foreground.
func (s *Service) paneAgentGroup(ctx context.Context, paneID string) (int, error) {
	raw, err := s.herdrCall(ctx, "pane.process_info", map[string]any{"pane_id": paneID})
	if err != nil {
		return 0, err
	}
	var result struct {
		ProcessInfo struct {
			ForegroundProcessGroupID int `json:"foreground_process_group_id"`
			ShellPID                 int `json:"shell_pid"`
		} `json:"process_info"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("decode pane process info: %w", err)
	}
	group := result.ProcessInfo.ForegroundProcessGroupID
	if group <= 1 || group == result.ProcessInfo.ShellPID {
		return 0, nil
	}
	return group, nil
}
