package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

type pluginEvent struct {
	Event string         `json:"event"`
	Data  map[string]any `json:"data"`
}

func (s *Service) ingest(ctx *axi.Context, args []string) error {
	if len(args) != 0 {
		return axi.Usage("_ingest does not take arguments")
	}
	if err := s.ingestEvent(ctx.Context); err != nil {
		if db, _, dbErr := s.openDB(); dbErr == nil {
			_ = db.AddEvent(context.Background(), "posse.ingest.error", os.Getenv("HERDR_PANE_ID"), marshalJSON(map[string]string{"error": err.Error()}), currentTime())
			_ = db.Close()
		}
		return ctx.Print(axi.Object{{Key: "ingested", Value: false}, {Key: "error", Value: err.Error()}, {Key: "help", Value: []any{"Run `posse doctor` to inspect Herdr and plugin setup"}}})
	}
	return ctx.Print(axi.Object{{Key: "ingested", Value: true}, {Key: "help", Value: []any{"Return control to Herdr after event processing"}}})
}

func (s *Service) RunIngest(args []string) int {
	ctx := &axi.Context{Context: context.Background(), Out: os.Stdout, ErrOut: os.Stderr}
	if err := s.ingest(ctx, args); err != nil {
		return 0
	}
	return 0
}

func (s *Service) ingestEvent(ctx context.Context) error {
	raw := os.Getenv("HERDR_PLUGIN_EVENT_JSON")
	var event pluginEvent
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return fmt.Errorf("decode Herdr plugin event: %w", err)
		}
	}
	if event.Event == "" {
		event.Event = os.Getenv("HERDR_PLUGIN_EVENT")
	}
	if event.Event == "" {
		return fmt.Errorf("missing Herdr plugin event")
	}
	paneID := valueString(event.Data, "pane_id")
	if paneID == "" {
		paneID = os.Getenv("HERDR_PANE_ID")
	}
	db, _, err := s.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.AddEvent(ctx, event.Event, paneID, raw, currentTime()); err != nil {
		return err
	}
	if err := db.ReleaseExpiredDeliveryClaims(ctx, currentTime()-deliveryClaimTimeout.Milliseconds()); err != nil {
		return err
	}
	projects, worker, err := s.ingestProjects(ctx, db, event, paneID)
	if err != nil || len(projects) == 0 {
		return err
	}
	focusEvent := eventIs(event.Event, "pane.focused")
	var failures []error
	for _, project := range projects {
		home, err := s.homePath()
		if err != nil {
			if focusEvent {
				failures = append(failures, err)
				continue
			}
			return err
		}
		cfg, err := config.Load(home, project.Name)
		if err != nil {
			if focusEvent {
				failures = append(failures, err)
				continue
			}
			return err
		}
		result, err := s.reconcileProject(ctx, db, project, cfg)
		if err != nil {
			if focusEvent {
				failures = append(failures, err)
				continue
			}
			return err
		}
		if err := s.reconcileTaskPanes(ctx, db, project, result.Snapshot); err != nil {
			if focusEvent {
				failures = append(failures, err)
				continue
			}
			return err
		}
		if err := s.reconcileIntents(ctx, db, project, cfg, result.Snapshot); err != nil {
			if focusEvent {
				failures = append(failures, err)
				continue
			}
			return err
		}
		status := valueString(event.Data, "agent_status")
		if focusEvent || worker && eventIs(event.Event, "pane.agent_status_changed") && (status == "idle" || status == "done" || status == "working") {
			if err := s.deliverQueuedMessagesWithConfig(ctx, db, project, result.Snapshot, cfg); err != nil {
				if focusEvent {
					failures = append(failures, err)
					continue
				}
				return err
			}
		}
		leadIdle := paneID != "" && paneID == project.LeadPaneID && (status == "idle" || status == "done")
		if leadIdle || focusEvent || len(result.Notices) > 0 {
			if err := s.deliverNoticesWithSnapshot(ctx, db, project, result.Snapshot); err != nil {
				if focusEvent {
					failures = append(failures, err)
					continue
				}
				return err
			}
		}
	}
	return errors.Join(failures...)
}

func (s *Service) ingestProjects(ctx context.Context, db *store.DB, event pluginEvent, paneID string) ([]store.Project, bool, error) {
	if eventIs(event.Event, "pane.focused") {
		projects, err := db.Projects(ctx)
		if err != nil {
			return nil, false, err
		}
		pending := make([]store.Project, 0, len(projects))
		for _, project := range projects {
			notices, err := db.UndeliveredNotices(ctx, project.ID)
			if err != nil {
				return nil, false, err
			}
			messages, err := db.HasQueuedMessages(ctx, project.ID)
			if err != nil {
				return nil, false, err
			}
			if len(notices) > 0 || messages {
				pending = append(pending, project)
			}
		}
		return pending, false, nil
	}
	if paneID != "" {
		if task, err := db.TaskByPane(ctx, paneID); err == nil {
			project, err := db.ProjectByID(ctx, task.ProjectID)
			return []store.Project{project}, true, err
		} else if !store.IsNotFound(err) {
			return nil, false, err
		}
		if project, err := db.ProjectByLeadPane(ctx, paneID); err == nil {
			return []store.Project{project}, false, nil
		} else if !store.IsNotFound(err) {
			return nil, false, err
		}
	}
	return nil, false, nil
}

func (s *Service) deliverNotices(ctx context.Context, db *store.DB, project store.Project) error {
	return s.deliverNoticesWithSnapshot(ctx, db, project, herdr.Snapshot{})
}

func (s *Service) deliverNoticesWithSnapshot(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot) error {
	if err := db.ReleaseExpiredDeliveryClaims(ctx, currentTime()-deliveryClaimTimeout.Milliseconds()); err != nil {
		return err
	}
	notices, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil {
		return err
	}
	if len(notices) == 0 {
		lastNotification, err := db.LastNoticeNotification(ctx, project.ID)
		if err != nil || lastNotification == 0 {
			return err
		}
		if project.HerdrWorkspaceID != "" {
			if _, err := s.herdrCall(ctx, "workspace.report_metadata", map[string]any{"workspace_id": project.HerdrWorkspaceID, "source": "posse", "tokens": map[string]string{"posse": ""}}); err != nil {
				return err
			}
		}
		return db.RecordNoticeNotification(ctx, project.ID, 0)
	}
	lastNotification, err := db.LastNoticeNotification(ctx, project.ID)
	if err != nil {
		return err
	}
	if len(snapshot.Panes) == 0 {
		snapshot, err = s.snapshot(ctx)
		if err != nil {
			return err
		}
	}
	lead, found := findAppPane(snapshot.Panes, project.LeadPaneID, project.LeadLabel)
	// Lead lookout plugins acknowledge these without involving the Lead. Do
	// not display a Herdr notification for a batch that needs no turn.
	if found && lead.Agent != "" && extensionOwnsNotices(s.leadNoticeDelivery(project, lead.Agent)) {
		home, err := s.homePath()
		if err != nil {
			return err
		}
		cfg, err := config.Load(home, project.Name)
		if err != nil {
			return configError(err)
		}
		if cfg.Lowkey.Lead && onlyQuietPiNotices(notices) {
			return nil
		}
	}
	if lastNotification == 0 || time.Since(time.UnixMilli(lastNotification)) >= 20*time.Second {
		_, notifyErr := s.herdrCall(ctx, "notification.show", map[string]any{"title": fmt.Sprintf("Posse: %d new notices", len(notices)), "body": project.Name})
		if notifyErr != nil {
			return notifyErr
		}
		if err := db.RecordNoticeNotification(ctx, project.ID, currentTime()); err != nil {
			return err
		}
		if project.HerdrWorkspaceID != "" {
			if _, err := s.herdrCall(ctx, "workspace.report_metadata", map[string]any{"workspace_id": project.HerdrWorkspaceID, "source": "posse", "tokens": map[string]string{"posse": fmt.Sprint(len(notices))}}); err != nil {
				return err
			}
		}
	}
	if !found || lead.Agent == "" {
		return nil
	}
	home, err := s.homePath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return configError(err)
	}
	delivery := s.leadNoticeDelivery(project, lead.Agent)
	// Lead plugins own their lookout and quiet routine ack. A typed fallback
	// could race them and expose a lowkey Notice in the conversation.
	if extensionOwnsNotices(delivery) {
		return nil
	}
	prompt := noticeWakeMessage(ctx, db, notices, cfg.Lowkey.Lead)
	if session := agentSessionID(lead.Agent, string(lead.AgentSession)); session != "" && delivery == config.NoticeDeliveryCodexQueue {
		delivered, err := s.deliverNoticeBatch(ctx, db, project, notices, func() error {
			return queueCodexPrompt(ctx, project.Root, session, prompt)
		})
		if delivered || err == nil {
			return err
		}
		// A failed queue leaves the Notices undelivered for the typed prompt below.
	}
	if (lead.AgentStatus == "idle" || lead.AgentStatus == "done") && !lead.Focused && snapshot.FocusedPaneID != lead.PaneID {
		_, err := s.deliverNoticeBatch(ctx, db, project, notices, func() error {
			return s.safePrompt(ctx, lead.PaneID, prompt)
		})
		return err
	}
	return nil
}

func extensionOwnsNotices(delivery string) bool {
	return delivery == config.NoticeDeliveryPiExtension || delivery == config.NoticeDeliveryOpenCodePlugin
}

func onlyQuietPiNotices(notices []store.Notice) bool {
	for _, notice := range notices {
		if notice.Kind != "pr_opened" {
			return false
		}
	}
	return len(notices) > 0
}

// deliverNoticeBatch claims the Notices, sends them with send, and marks them
// delivered. A pane_focused refusal rolls the claim back without an error.
func (s *Service) deliverNoticeBatch(ctx context.Context, db *store.DB, project store.Project, notices []store.Notice, send func() error) (bool, error) {
	token, err := newDeliveryClaimToken()
	if err != nil {
		return false, err
	}
	ids := noticeIDs(notices)
	claimed, err := db.ClaimNoticeBatch(ctx, project.ID, ids, token, currentTime())
	if err != nil || !claimed {
		return false, err
	}
	if err := send(); err != nil {
		rollbackErr := db.RollbackNoticeClaim(ctx, project.ID, ids, token)
		if failure, ok := err.(*axi.Error); !ok || failure.Code != "pane_focused" {
			return false, errors.Join(err, rollbackErr)
		}
		return false, rollbackErr
	}
	if err := db.MarkClaimedNoticesDelivered(ctx, project.ID, ids, token, currentTime()); err != nil {
		return false, err
	}
	if project.HerdrWorkspaceID != "" {
		if _, err := s.herdrCall(ctx, "workspace.report_metadata", map[string]any{"workspace_id": project.HerdrWorkspaceID, "source": "posse", "tokens": map[string]string{"posse": ""}}); err != nil {
			return true, err
		}
	}
	return true, db.RecordNoticeNotification(ctx, project.ID, 0)
}

// leadNoticeDelivery returns the Notice delivery mode of the Lead's running
// kind, or prompt when the config cannot be read.
func (s *Service) leadNoticeDelivery(project store.Project, kind string) string {
	home, err := s.homePath()
	if err != nil {
		return config.NoticeDeliveryPrompt
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return config.NoticeDeliveryPrompt
	}
	return noticeDelivery(cfg.Kinds[kind])
}

func (s *Service) deliverQueuedMessages(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot) error {
	home, err := s.homePath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		return err
	}
	return s.deliverQueuedMessagesWithConfig(ctx, db, project, snapshot, cfg)
}

func (s *Service) deliverQueuedMessagesWithConfig(ctx context.Context, db *store.DB, project store.Project, snapshot herdr.Snapshot, cfg config.Config) error {
	if err := db.ReleaseExpiredDeliveryClaims(ctx, currentTime()-deliveryClaimTimeout.Milliseconds()); err != nil {
		return err
	}
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if (task.State == store.StateDone && task.Type != "ship") || task.State == store.StateReported || task.State == store.StateLanded || task.State == store.StateTornDown || task.State == store.StateFailed || task.State == store.StateLost {
			continue
		}
		message, err := db.OldestQueuedMessage(ctx, task.ID)
		if err == store.ErrNotFound {
			continue
		}
		if err != nil {
			return err
		}
		pane, found := findAppPane(snapshot.Panes, task.PaneID, task.PaneLabel)
		if !found || queuedMessageReason(task, message, pane, snapshot, cfg) != "" {
			continue
		}
		delivered, _, err := s.deliverClaimedMessage(ctx, db, task, message, pane.PaneID, cfg)
		if err != nil {
			return err
		}
		if !delivered {
			continue
		}
		if task.State == store.StateNeedsDecision {
			if err := db.Transition(ctx, task.ID, store.StateNeedsDecision, store.StateWorking, "lead", "Lead delivered a response"); err != nil {
				return err
			}
		} else if task.State == store.StateDone && task.Type == "ship" {
			if err := db.Transition(ctx, task.ID, store.StateDone, store.StateWorking, "lead", "Lead delivered a fix instruction"); err != nil {
				return err
			}
			if err := db.ClearTaskGatedSHA(ctx, task.ID); err != nil {
				return err
			}
		} else if task.State == store.StateLanding && task.Type == "ship" {
			if err := db.Transition(ctx, task.ID, store.StateLanding, store.StateWorking, "lead", "Lead delivered a pull request fix instruction"); err != nil {
				return err
			}
			if err := db.ClearTaskGatedSHA(ctx, task.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func noticeIDs(notices []store.Notice) []int64 {
	ids := make([]int64, len(notices))
	for i, notice := range notices {
		ids[i] = notice.ID
	}
	return ids
}

func noticeRows(ctx context.Context, db *store.DB, notices []store.Notice) []noticeSummary {
	rows := make([]noticeSummary, 0, len(notices))
	for _, notice := range notices {
		rows = append(rows, noticeSummary{ID: notice.ID, Task: noticeTaskTitle(ctx, db, notice.ProjectID, notice.TaskID), Kind: notice.Kind, Summary: notice.Summary})
	}
	return rows
}

func eventIs(value, expected string) bool {
	switch value {
	case "pane_agent_status_changed":
		return expected == "pane.agent_status_changed"
	case "pane_exited":
		return expected == "pane.exited"
	case "pane_closed":
		return expected == "pane.closed"
	case "pane_focused":
		return expected == "pane.focused"
	default:
		return value == expected
	}
}

func valueString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func marshalJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}
