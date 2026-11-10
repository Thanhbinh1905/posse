package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/execgroup"
	"github.com/thanhbinh1905/posse/internal/store"
)

const noticeCommandTimeout = 5 * time.Second

type noticeCommandItem struct {
	ID          int64           `json:"id"`
	ProjectID   int64           `json:"project_id"`
	TaskID      int64           `json:"task_id"`
	Kind        string          `json:"kind"`
	Summary     string          `json:"summary"`
	Data        json.RawMessage `json:"data"`
	CreatedAt   int64           `json:"created_at"`
	DeliveredAt int64           `json:"delivered_at"`
	AckedAt     int64           `json:"acked_at"`
}

func runNoticeCommand(command string, notices []store.Notice, mergeAutonomy string) {
	runNoticeCommandWithLogger(command, notices, mergeAutonomy, log.Printf)
}

func runNoticeCommandWithLogger(command string, notices []store.Notice, mergeAutonomy string, logf func(string, ...any)) {
	command = strings.TrimSpace(command)
	if command == "" {
		return
	}
	selected := make([]noticeCommandItem, 0, len(notices))
	for _, notice := range notices {
		if !noticeNeedsUser(notice, mergeAutonomy) {
			continue
		}
		selected = append(selected, noticeCommandItem{
			ID: notice.ID, ProjectID: notice.ProjectID, TaskID: notice.TaskID,
			Kind: notice.Kind, Summary: notice.Summary, Data: json.RawMessage(notice.DataJSON), CreatedAt: notice.CreatedAt,
			DeliveredAt: notice.DeliveredAt, AckedAt: notice.AckedAt,
		})
	}
	if len(selected) == 0 {
		return
	}
	payload, err := json.Marshal(selected)
	if err != nil {
		logf("posse: could not encode notify command payload: %v", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), noticeCommandTimeout)
		defer cancel()
		cmd := execgroup.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			logf("posse: notify command failed: %v", err)
		}
	}()
}

func (s *Service) dispatchNoticeCommand(ctx context.Context, db *store.DB, project store.Project, notices []store.Notice) {
	home, err := s.homePath()
	if err != nil {
		log.Printf("posse: could not resolve notify command config for Project %s: %v", project.Name, err)
		return
	}
	cfg, err := config.Load(home, project.Name)
	if err != nil {
		log.Printf("posse: could not load notify command config for Project %s: %v", project.Name, err)
		return
	}
	if strings.TrimSpace(cfg.Notify.Command) == "" {
		return
	}
	var actionable []store.Notice
	for _, notice := range notices {
		if noticeNeedsUser(notice, cfg.Autonomy.Merge) {
			actionable = append(actionable, notice)
		}
	}
	if len(actionable) == 0 {
		return
	}
	claimed, err := db.ClaimNoticeCommandNotifications(ctx, project.ID, actionable, currentTime())
	if err != nil {
		log.Printf("posse: could not record notify command Notices for Project %s: %v", project.Name, err)
	}
	runNoticeCommand(cfg.Notify.Command, claimed, cfg.Autonomy.Merge)
}

func noticeNeedsUser(notice store.Notice, mergeAutonomy string) bool {
	switch notice.Kind {
	case "needs_decision", "message_delivery_uncertain", "notice_delivery_uncertain", "lead_receiver_missing",
		"task_failed", "task_lost", "quota_exhausted", "pr_closed",
		"pr_merged_ungated", "review_policy_violation", "head_discontinuity",
		"task_done", "pr_merged":
		return true
	case "root_fetch_failing", "pr_watch_failing":
		var data struct {
			ConsecutiveFailures int `json:"consecutive_failures"`
		}
		return json.Unmarshal([]byte(notice.DataJSON), &data) == nil && data.ConsecutiveFailures >= 3
	case "merge_ready":
		return mergeAutonomy == "" || mergeAutonomy == "ask"
	default:
		return false
	}
}
