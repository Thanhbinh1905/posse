package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thanhbinh1905/posse/internal/store"
)

type greenhouseWatchSnapshot struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Stage  string `json:"stage"`
	Run    struct {
		StartedAt      string `json:"started_at"`
		StageStartedAt string `json:"stage_started_at"`
		ID             string `json:"id"`
		Status         string `json:"status"`
		Stage          string `json:"stage"`
		RepairUsed     int    `json:"repair_used"`
		RepairLimit    int    `json:"repair_limit"`
		Pipeline       *struct {
			RepairUsed  int `json:"repair_used"`
			RepairLimit int `json:"repair_limit"`
			Stages      []struct {
				Name       string `json:"name"`
				StartedAt  string `json:"started_at"`
				FinishedAt string `json:"finished_at"`
			} `json:"stages"`
			Active *struct {
				Stage string `json:"stage"`
			} `json:"active"`
		} `json:"pipeline"`
	} `json:"run"`
}

func greenhouseStageLabel(snapshot greenhouseWatchSnapshot) (string, string) {
	status := snapshot.Status
	if snapshot.Run.Status != "" {
		status = snapshot.Run.Status
	}
	stage := snapshot.Stage
	if snapshot.Run.Stage != "" {
		stage = snapshot.Run.Stage
	}
	if snapshot.Run.Pipeline != nil && snapshot.Run.Pipeline.Active != nil && snapshot.Run.Pipeline.Active.Stage != "" {
		stage = snapshot.Run.Pipeline.Active.Stage
	}
	if stage == "pipeline" && status == "running" {
		stage = "review"
	}
	return stage, status
}

func greenhouseElapsedSummary(watch store.GreenhouseTaskWatch, now time.Time) string {
	runStart := watch.RunStartedAt
	if runStart == 0 {
		runStart = watch.RunObservedAt
	}
	stageStart := watch.StageStartedAt
	if stageStart == 0 {
		stageStart = watch.StageObservedAt
	}
	stageEnd := watch.StageFinishedAt
	if stageEnd > 0 {
		now = time.UnixMilli(stageEnd)
	}
	return watch.Stage + " " + greenhouseElapsedLabel(time.UnixMilli(stageStart), now) + " · run " + greenhouseElapsedLabel(time.UnixMilli(runStart), now)
}

func greenhouseTimestampMillis(value string) int64 {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

func greenhouseStageTimestamps(snapshot greenhouseWatchSnapshot, stage string) (int64, int64, int64) {
	runStarted := greenhouseTimestampMillis(snapshot.Run.StartedAt)
	stageStarted := greenhouseTimestampMillis(snapshot.Run.StageStartedAt)
	var stageFinished int64
	if snapshot.Run.Pipeline != nil {
		for _, result := range snapshot.Run.Pipeline.Stages {
			if result.Name == stage {
				if stageStarted == 0 {
					stageStarted = greenhouseTimestampMillis(result.StartedAt)
				}
				stageFinished = greenhouseTimestampMillis(result.FinishedAt)
				break
			}
		}
	}
	return runStarted, stageStarted, stageFinished
}

func greenhouseElapsedLabel(start, now time.Time) string {
	if start.IsZero() || now.Before(start) {
		return "0s"
	}
	elapsed := now.Sub(start).Round(time.Second)
	if elapsed < time.Minute {
		return elapsed.String()
	}
	return fmt.Sprintf("%dm", int(elapsed.Round(time.Minute)/time.Minute))
}

func greenhouseTaskStatus(ctx context.Context, db *store.DB, task store.Task, now time.Time) (string, error) {
	watch, err := db.GreenhouseTaskWatch(ctx, task.ID)
	if store.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if watch.Status == "ci-ready" || watch.Status == "locally-ready" {
		return "PR green", nil
	}
	if watch.Status == "needs-decision" {
		return "waiting for decision", nil
	}
	if watch.Status == "failed" || watch.Status == "recovery-required" {
		return "failed: " + watch.Stage, nil
	}
	return greenhouseElapsedSummary(watch, now), nil
}

func greenhouseRunIDFromSignals(ctx context.Context, db *store.DB, task store.Task) string {
	signals, err := db.TaskSignals(ctx, task.ID, 20)
	if err != nil {
		return ""
	}
	for _, signal := range signals {
		if signal.Verb != "done" {
			continue
		}
		var data struct {
			RunID string `json:"greenhouse_run_id"`
		}
		if json.Unmarshal([]byte(signal.DataJSON), &data) == nil && data.RunID != "" {
			return data.RunID
		}
	}
	return ""
}

func inspectGreenhouseTask(ctx context.Context, task store.Task) (greenhouseWatchSnapshot, error) {
	request := []byte(`{"version":2,"op":"inspect"}`)
	command := exec.CommandContext(ctx, "greenhouse", "axi")
	command.Dir = task.WorktreePath
	command.Stdin = bytes.NewReader(request)
	command.Env = os.Environ()
	output, err := command.Output()
	if err != nil {
		return greenhouseWatchSnapshot{}, fmt.Errorf("inspect Greenhouse in %s: %w", task.WorktreePath, err)
	}
	var snapshot greenhouseWatchSnapshot
	if err := json.Unmarshal(output, &snapshot); err != nil {
		return snapshot, fmt.Errorf("decode Greenhouse inspect: %w", err)
	}
	if snapshot.RunID == "" {
		snapshot.RunID = snapshot.Run.ID
	}
	return snapshot, nil
}

func (s *Service) observeGreenhouseTasks(ctx context.Context, db *store.DB, project store.Project) error {
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.MergeMode != "greenhouse" || task.WorktreePath == "" || task.State == store.StateTornDown {
			continue
		}
		watch, err := db.GreenhouseTaskWatch(ctx, task.ID)
		previousStatus := watch.Status
		if err != nil && !store.IsNotFound(err) {
			return err
		}
		runID := watch.RunID
		if runID == "" {
			runID = greenhouseRunIDFromSignals(ctx, db, task)
		}
		var snapshot greenhouseWatchSnapshot
		if runID != "" {
			snapshot = inspectGreenhouseRun(ctx, task, runID)
			if snapshot.RunID == "" {
				snapshot.RunID = runID
			}
		} else {
			snapshot, err = inspectGreenhouseTask(ctx, task)
			if err != nil || snapshot.RunID == "" {
				continue
			}
		}
		stage, status := greenhouseStageLabel(snapshot)
		if status == "" {
			continue
		}
		now := time.Now().UnixMilli()
		runStarted, stageStarted, stageFinished := greenhouseStageTimestamps(snapshot, stage)
		if watch.RunID != snapshot.RunID {
			watch = store.GreenhouseTaskWatch{TaskID: task.ID, RunID: snapshot.RunID, RunObservedAt: now, StageObservedAt: now}
		}
		if stage != watch.Stage {
			watch.StageObservedAt = now
		}
		if runStarted > 0 {
			watch.RunStartedAt = runStarted
		}
		if stageStarted > 0 {
			watch.StageStartedAt = stageStarted
		} else if stage != watch.Stage {
			watch.StageStartedAt = 0
		}
		if stageFinished > 0 {
			watch.StageFinishedAt = stageFinished
		} else if stage != watch.Stage {
			watch.StageFinishedAt = 0
			if stageStarted == 0 {
				watch.StageStartedAt = 0
			}
		}
		watch.Status, watch.Stage, watch.UpdatedAt = status, stage, now
		if err := db.SaveGreenhouseTaskWatch(ctx, watch); err != nil {
			return err
		}
		if previousStatus != watch.Status {
			if err := raiseGreenhouseNotice(ctx, db, project, task, snapshot, previousStatus, watch.Status); err != nil {
				return err
			}
		}
		if !greenhousePaneExists(ctx, s, task, watch.PaneID) {
			watch.PaneID = ""
		}
		if watch.PaneID == "" && watch.Status != "ci-ready" && watch.Status != "locally-ready" && task.HerdrWorkspaceID != "" && s.Herdr != nil {
			paneID, err := s.openGreenhousePane(ctx, task)
			if err == nil {
				watch.PaneID = paneID
				if err := db.SaveGreenhouseTaskWatch(ctx, watch); err != nil {
					return err
				}
			}
		}
		s.reportGreenhouseTaskMetadata(ctx, db, project, task, watch, snapshot)
	}
	return nil
}

func raiseGreenhouseNotice(ctx context.Context, db *store.DB, project store.Project, task store.Task, snapshot greenhouseWatchSnapshot, previous, current string) error {
	kind, summary := "", ""
	stage, _ := greenhouseStageLabel(snapshot)
	switch current {
	case "needs-decision":
		kind, summary = "greenhouse_decision_needed", task.Title+": Greenhouse needs a decision"
	case "failed", "recovery-required":
		kind, summary = "greenhouse_failed", task.Title+": Greenhouse failed: "+stage
	case "ci-ready", "locally-ready":
		kind, summary = "greenhouse_pr_green", task.Title+": Greenhouse pipeline is green"
	}
	if kind == "" || previous == current {
		return nil
	}
	_, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: kind, Summary: summary, DataJSON: marshalJSON(map[string]any{"run_id": snapshot.RunID, "stage": stage})})
	return err
}

func inspectGreenhouseRun(ctx context.Context, task store.Task, runID string) greenhouseWatchSnapshot {
	request, _ := json.Marshal(map[string]any{"version": 2, "op": "inspect", "run_id": runID})
	command := exec.CommandContext(ctx, "greenhouse", "axi")
	command.Dir = task.WorktreePath
	command.Stdin = bytes.NewReader(request)
	command.Env = os.Environ()
	output, err := command.Output()
	if err != nil {
		return greenhouseWatchSnapshot{}
	}
	var snapshot greenhouseWatchSnapshot
	if json.Unmarshal(output, &snapshot) != nil {
		return greenhouseWatchSnapshot{}
	}
	if snapshot.RunID == "" {
		snapshot.RunID = snapshot.Run.ID
	}
	return snapshot
}

func (s *Service) reportGreenhouseTaskMetadata(ctx context.Context, db *store.DB, project store.Project, task store.Task, watch store.GreenhouseTaskWatch, snapshot greenhouseWatchSnapshot) {
	if task.HerdrWorkspaceID == "" || s.Herdr == nil {
		return
	}
	now := time.Now()
	label := greenhouseElapsedSummary(watch, now)
	if snapshot.Run.Pipeline != nil && snapshot.Run.Pipeline.RepairLimit > 0 {
		label = watch.Stage + " · fix " + fmt.Sprintf("%d/%d", snapshot.Run.Pipeline.RepairUsed, snapshot.Run.Pipeline.RepairLimit)
	}
	if watch.Status == "needs-decision" {
		label = "waiting for decision"
	} else if watch.Status == "ci-ready" || watch.Status == "locally-ready" {
		label = "PR green"
	} else if watch.Status == "failed" || watch.Status == "recovery-required" {
		label = "failed: " + strings.TrimSpace(watch.Stage)
	}
	notices, err := db.UndeliveredNotices(ctx, project.ID)
	if err != nil {
		return
	}
	_, _ = s.herdrCall(ctx, "workspace.report_metadata", map[string]any{
		"workspace_id": task.HerdrWorkspaceID, "source": "posse",
		"tokens": map[string]string{"posse": fmt.Sprint(len(notices)), "greenhouse": label},
	})
}

func greenhousePaneExists(ctx context.Context, s *Service, task store.Task, paneID string) bool {
	if paneID == "" || s.Herdr == nil {
		return false
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return false
	}
	for _, pane := range snapshot.Panes {
		if pane.PaneID == paneID && pane.WorkspaceID == task.HerdrWorkspaceID {
			return true
		}
	}
	return false
}

func (s *Service) openGreenhousePane(ctx context.Context, task store.Task) (string, error) {
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return "", err
	}
	for _, pane := range snapshot.Panes {
		if pane.WorkspaceID == task.HerdrWorkspaceID && filepath.Clean(pane.CWD) == filepath.Clean(task.WorktreePath) && pane.PaneID != task.PaneID && strings.Contains(pane.Label, "greenhouse") {
			return pane.PaneID, nil
		}
	}
	raw, err := s.herdrCall(ctx, "pane.split", map[string]any{"target_pane_id": task.PaneID, "direction": "right", "cwd": task.WorktreePath, "focus": false})
	if err != nil {
		return "", err
	}
	var result struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Pane.PaneID == "" {
		return "", fmt.Errorf("herdr did not return the Greenhouse pane")
	}
	paneID := result.Pane.PaneID
	if _, err := s.herdrCall(ctx, "pane.rename", map[string]any{"pane_id": paneID, "label": "greenhouse:" + task.PaneLabel}); err != nil {
		return "", err
	}
	if _, err := s.herdrCall(ctx, "pane.send_input", map[string]any{"pane_id": paneID, "text": "greenhouse", "keys": []string{"enter"}}); err != nil {
		return "", err
	}
	return paneID, nil
}
