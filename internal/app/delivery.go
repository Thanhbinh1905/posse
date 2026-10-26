package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

const deliveryClaimTimeout = 2 * time.Minute

var deliveryClaimRefreshInterval = 30 * time.Second

func keepMessageDeliveryClaimsAlive(ctx context.Context, db *store.DB, taskID, messageID int64, token string) (context.Context, func() error) {
	reservationCtx, cancel := context.WithCancel(ctx)
	interval := deliveryClaimRefreshInterval
	if interval <= 0 {
		interval = deliveryClaimTimeout / 3
	}
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-reservationCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := db.RefreshMessageDeliveryClaims(reservationCtx, taskID, messageID, token, currentTime()); err != nil {
					if reservationCtx.Err() != nil {
						done <- nil
					} else {
						done <- err
						cancel()
					}
					return
				}
			}
		}
	}()
	return reservationCtx, func() error {
		cancel()
		return <-done
	}
}

func (s *Service) raiseExpiredMessageDeliveryNotices(ctx context.Context, db *store.DB, project store.Project) (bool, error) {
	submissions, err := db.ExpiredMessageSubmissions(ctx, project.ID, 0, currentTime()-deliveryClaimTimeout.Milliseconds())
	if err != nil {
		return false, err
	}
	createdAny := false
	for _, submission := range submissions {
		created, err := s.createMessageDeliveryUncertainNotice(ctx, db, project.ID, submission.TaskSeq, submission.Message)
		if err != nil {
			return createdAny, err
		}
		createdAny = createdAny || created
	}
	if createdAny {
		if err := s.regenerateProjects(ctx, db); err != nil {
			return true, err
		}
	}
	return createdAny, nil
}

func (s *Service) createMessageDeliveryUncertainNotice(ctx context.Context, db *store.DB, projectID int64, taskSeq int, message store.Message) (bool, error) {
	task := taskIDString(taskSeq)
	preview := clipRunes(strings.Join(strings.Fields(message.Body), " "), 48)
	summary := fmt.Sprintf("Message #%d for %s may not have reached the Worker. Inspect with `posse peek %s` and `posse show %s --full`; send a replacement only if absent. Instruction: %q", message.ID, task, task, task, preview)
	created, err := db.CreateMessageDeliveryUncertainNotice(ctx, projectID, message.TaskID, message.ID, summary, message.Body)
	return created, err
}

// An empty reason means it is safe to submit now. The persisted message policy
// is checked on every retry so --queue cannot turn into a mid-turn steer.
func (s *Service) piCompactingPane(ctx context.Context, pane herdr.Pane) (herdr.Pane, bool, error) {
	if pane.Agent != "pi" || pane.AgentStatus != "unknown" {
		return pane, false, nil
	}
	raw, err := s.herdrCall(ctx, "pane.read", map[string]any{"pane_id": pane.PaneID, "source": "visible"})
	if err != nil {
		return pane, false, herdrError(err)
	}
	var screen struct {
		Text string `json:"text"`
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	if err := json.Unmarshal(raw, &screen); err != nil {
		return pane, false, err
	}
	output := screen.Read.Text
	if output == "" {
		output = screen.Text
	}
	if !piAutoCompacting(output) {
		return pane, false, nil
	}
	pane.AgentStatus = "working"
	return pane, true, nil
}

func piAutoCompacting(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "auto-compacting...", "auto-compacting…", "auto compacting...", "auto compacting…":
			return true
		}
	}
	return false
}

func deliveredMessageReason(pane herdr.Pane, compacting bool) string {
	if compacting || pane.AgentStatus == "working" {
		return "steered"
	}
	return "agent_ready"
}

func canSteerPane(task store.Task, pane herdr.Pane, snapshot herdr.Snapshot, cfg config.Config, allowFocused bool) bool {
	focused := pane.Focused || snapshot.FocusedPaneID == pane.PaneID
	return pane.AgentStatus == "working" && (allowFocused || !focused) &&
		task.State != store.StateBlocked && pane.Agent != "" && cfg.Kinds[pane.Agent].Steer
}

func queuedMessageReason(task store.Task, message store.Message, pane herdr.Pane, snapshot herdr.Snapshot, cfg config.Config, allowFocused bool) string {
	if !allowFocused && (pane.Focused || snapshot.FocusedPaneID == pane.PaneID) {
		return "focused"
	}
	if task.State == store.StateBlocked || pane.AgentStatus == "blocked" {
		return "blocked"
	}
	if pane.Agent == "" {
		return "agent_not_ready"
	}
	if pane.AgentStatus == "idle" || pane.AgentStatus == "done" {
		return ""
	}
	if message.WaitForIdle {
		return "waiting_for_idle (--queue)"
	}
	if !cfg.Kinds[pane.Agent].Steer {
		return "kind_does_not_support_steering"
	}
	if pane.AgentStatus == "working" {
		return ""
	}
	return "agent_not_ready"
}

// Dialog key hints near the bottom of a detection snapshot indicate that Enter
// selects a menu item rather than submitting the prompt editor.
func inputDialogVisible(screen string) bool {
	lines := strings.Split(screen, "\n")
	for _, line := range lines[max(0, len(lines)-8):] {
		line = strings.ToLower(line)
		if strings.Contains(line, "enter to ") && (strings.Contains(line, "esc") || strings.Contains(line, "cancel")) {
			return true
		}
	}
	return false
}

func newDeliveryClaimToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Service) deliverClaimedMessage(ctx context.Context, db *store.DB, task store.Task, message store.Message, paneID string, cfg config.Config, allowFocused bool) (bool, string, error) {
	at := currentTime()
	active, err := db.HasDeliveryInProgress(ctx, task.ID, at-deliveryClaimTimeout.Milliseconds())
	if err != nil || active {
		if active && err == nil {
			return false, "delivery_in_progress", nil
		}
		return false, "", err
	}
	token, err := newDeliveryClaimToken()
	if err != nil {
		return false, "", err
	}
	claimed, err := db.ClaimMessage(ctx, message.ID, token, at, at-deliveryClaimTimeout.Milliseconds())
	if err != nil || !claimed {
		return false, "", err
	}
	claimCtx, stopClaim := keepMessageDeliveryClaimsAlive(ctx, db, task.ID, message.ID, token)
	delivered, reason, submitErr := s.submitClaimedMessage(claimCtx, db, task, message, paneID, cfg, token, allowFocused)
	return delivered, reason, errors.Join(submitErr, stopClaim())
}

func (s *Service) submitClaimedMessage(ctx context.Context, db *store.DB, task store.Task, message store.Message, paneID string, cfg config.Config, token string, allowFocused bool) (bool, string, error) {
	params := map[string]any{"target": paneID, "text": workerInstruction(task, message)}
	// Only a prompt into an idle agent starts an observable turn. Steering a
	// working agent has no state change that confirms its submission.
	confirmTurn := false
	check := func(snapshot herdr.Snapshot) error {
		pane, found := findAppPane(snapshot.Panes, paneID, task.PaneLabel)
		if !found || pane.PaneID != paneID {
			return axi.Failure("message_not_ready", "Worker is not ready for this message", true)
		}
		var err error
		pane, _, err = s.piCompactingPane(ctx, pane)
		if err != nil {
			return err
		}
		if queuedMessageReason(task, message, pane, snapshot, cfg, allowFocused) != "" {
			return axi.Failure("message_not_ready", "Worker is not ready for this message", true)
		}
		confirmTurn = pane.AgentStatus == "idle" || pane.AgentStatus == "done"
		// Herdr can keep the previous idle/done status when a modal matches
		// an unknown detection rule (for example Claude's model picker).
		// Check the live TUI classification before submitting an Enter.
		raw, err := s.herdrCall(ctx, "agent.explain", map[string]any{"target": paneID})
		if err != nil {
			return err
		}
		var response struct {
			Explain struct {
				State                  string `json:"state"`
				ScreenDetectionSkipped bool   `json:"screen_detection_skipped"`
			} `json:"explain"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return err
		}
		if response.Explain.State == "blocked" || response.Explain.State == "unknown" && !response.Explain.ScreenDetectionSkipped {
			return axi.Failure("agent_ui_unknown", "Worker input is in a dialog or an unrecognized UI", true)
		}
		if response.Explain.ScreenDetectionSkipped {
			// Pi's native status remains idle while its model picker is open;
			// unlike Claude, Herdr skips screen classification for this kind.
			raw, err := s.herdrCall(ctx, "pane.read", map[string]any{"pane_id": paneID, "source": "detection"})
			if err != nil {
				return err
			}
			var screen struct {
				Read struct {
					Text string `json:"text"`
				} `json:"read"`
			}
			if err := json.Unmarshal(raw, &screen); err != nil {
				return err
			}
			if screen.Read.Text == "" || inputDialogVisible(screen.Read.Text) {
				return axi.Failure("agent_ui_unknown", "Worker input is in a dialog or an unrecognized UI", true)
			}
		}
		return nil
	}
	submitting := false
	beforePrompt := func() error {
		if err := db.MarkMessageSubmitting(ctx, message.ID, token); err != nil {
			return err
		}
		submitting = true
		if confirmTurn {
			params["wait"] = map[string]any{"until": []string{"working", "blocked"}, "timeout_ms": messageSubmitWait.Milliseconds()}
		}
		return nil
	}
	send := func(ctx context.Context, method string, params map[string]any, beforeSend func() error) error {
		return s.typeIntoBeforeWithSubmissionGate(ctx, paneID, method, params, check, beforeSend, nil, allowFocused)
	}
	promptErr := send(ctx, "agent.prompt", params, beforePrompt)
	if promptErr != nil && submitting && confirmTurn && isPromptStall(promptErr) {
		promptErr = s.confirmMessageSubmitted(ctx, paneID, send)
	}
	if err := promptErr; err != nil {
		if submitting {
			created, noticeErr := s.createMessageDeliveryUncertainNotice(ctx, db, task.ProjectID, task.Seq, message)
			if noticeErr == nil && created {
				noticeErr = s.regenerateProjects(ctx, db)
			}
			uncertainErr := axi.Failure("message_delivery_uncertain", "Herdr may have submitted this instruction, so Posse will not retry it automatically", false, "Inspect the Worker pane before sending another instruction")
			if noticeErr != nil {
				return false, "", errors.Join(uncertainErr, noticeErr)
			}
			return false, "", uncertainErr
		}
		var rollbackErr error
		if message.Status == "interrupting" {
			rollbackErr = db.RollbackInterruptMessage(ctx, message.ID, token)
		} else {
			rollbackErr = db.RollbackMessageClaim(ctx, message.ID, token)
		}
		if failure, ok := err.(*axi.Error); ok {
			switch failure.Code {
			case "pane_focused":
				return false, "focused", rollbackErr
			case "agent_blocked":
				return false, "blocked", rollbackErr
			case "agent_ui_unknown":
				return false, "agent_ui_unknown", rollbackErr
			case "message_not_ready":
				return false, "agent_not_ready", rollbackErr
			}
		}
		return false, "", errors.Join(err, rollbackErr)
	}
	if err := db.MarkMessageDelivered(ctx, message.ID, token, currentTime()); err != nil {
		return false, "", err
	}
	if err := db.PersistTask(ctx, task.ID); err != nil {
		return true, "", err
	}
	return true, "", nil
}

var (
	messageSubmitAttempts = 3
	messageSubmitWait     = 5 * time.Second
)

// confirmMessageSubmitted handles a prompt that started no turn. Claude Code
// collapses a long or multi-line paste into a "[Pasted text #N]" placeholder
// and can swallow an Enter that arrives while the placeholder is created,
// leaving the instruction unsubmitted. While that placeholder is visible,
// Posse presses Enter again; the text is never retyped, because a second copy
// could reach the Worker twice. A loaded agent can also start its turn after
// the first wait, so each attempt waits for activity again, even when the
// input guard refuses the Enter because the User focused the pane or the
// agent left idle. A placeholder that survives every attempt without a turn
// makes delivery uncertain. Without one, nothing shows the accepted prompt
// was lost.
func (s *Service) confirmMessageSubmitted(ctx context.Context, paneID string, send func(context.Context, string, map[string]any, func() error) error) error {
	var lastErr error
	for attempt := 1; attempt <= messageSubmitAttempts; attempt++ {
		if s.agentActive(ctx, paneID) || !s.pastePlaceholderOnScreen(ctx, paneID) {
			return nil
		}
		if err := send(ctx, "agent.send_keys", map[string]any{"target": paneID, "keys": []string{"enter"}}, nil); err != nil && !inputGuardRefusal(err) {
			return err
		}
		waitCtx, cancel := context.WithTimeout(ctx, messageSubmitWait+5*time.Second)
		_, lastErr = s.herdrCall(waitCtx, "agent.wait", map[string]any{"target": paneID, "until": []string{"working", "blocked"}, "timeout_ms": messageSubmitWait.Milliseconds()})
		cancel()
		if lastErr == nil {
			return nil
		}
		if !isPromptStall(lastErr) {
			return lastErr
		}
	}
	return axi.Failure("message_not_submitted", fmt.Sprintf("the Worker input still holds a paste placeholder after %d submit attempts: %v", messageSubmitAttempts, lastErr), false)
}

// inputGuardRefusal reports an error from the checks that run before Posse
// types into a pane. Nothing reached the pane, so the caller may wait instead.
func inputGuardRefusal(err error) bool {
	var failure *axi.Error
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Code {
	case "pane_focused", "agent_blocked", "agent_ui_unknown", "message_not_ready":
		return true
	}
	return false
}
