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
	summary := fmt.Sprintf("Message #%d for %s may not have reached the Rider. Inspect with `posse peek %s` and `posse show %s --full`; send a replacement only if absent. Instruction: %q", message.ID, task, task, task, preview)
	created, err := db.CreateMessageDeliveryUncertainNotice(ctx, projectID, message.TaskID, message.ID, summary, message.Body)
	return created, err
}

// An empty reason means it is safe to submit now. The persisted message policy
// is checked on every retry so --queue cannot turn into a mid-turn steer.
func queuedMessageReason(task store.Task, message store.Message, pane herdr.Pane, snapshot herdr.Snapshot, cfg config.Config) string {
	if pane.Focused || snapshot.FocusedPaneID == pane.PaneID {
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

func (s *Service) deliverClaimedMessage(ctx context.Context, db *store.DB, task store.Task, message store.Message, paneID string, cfg config.Config) (bool, string, error) {
	token, err := newDeliveryClaimToken()
	if err != nil {
		return false, "", err
	}
	claimed, err := db.ClaimMessage(ctx, message.ID, token, currentTime())
	if err != nil || !claimed {
		return false, "", err
	}
	check := func(snapshot herdr.Snapshot) error {
		pane, found := findAppPane(snapshot.Panes, paneID, task.PaneLabel)
		if !found || pane.PaneID != paneID || queuedMessageReason(task, message, pane, snapshot, cfg) != "" {
			return axi.Failure("message_not_ready", "Rider is not ready for this message", true)
		}
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
			return axi.Failure("agent_ui_unknown", "Rider input is in a dialog or an unrecognized UI", true)
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
				return axi.Failure("agent_ui_unknown", "Rider input is in a dialog or an unrecognized UI", true)
			}
		}
		return nil
	}
	submitting := false
	if err := s.safePromptWhenBefore(ctx, paneID, workerInstruction(task, message), check, func() error {
		if err := db.MarkMessageSubmitting(ctx, message.ID, token); err != nil {
			return err
		}
		submitting = true
		return nil
	}); err != nil {
		if submitting {
			created, noticeErr := s.createMessageDeliveryUncertainNotice(ctx, db, task.ProjectID, task.Seq, message)
			if noticeErr == nil && created {
				noticeErr = s.regenerateProjects(ctx, db)
			}
			uncertainErr := axi.Failure("message_delivery_uncertain", "Herdr may have submitted this instruction, so Posse will not retry it automatically", false, "Inspect the Rider pane before sending another instruction")
			if noticeErr != nil {
				return false, "", errors.Join(uncertainErr, noticeErr)
			}
			return false, "", uncertainErr
		}
		rollbackErr := db.RollbackMessageClaim(ctx, message.ID, token)
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
	return true, "", nil
}
