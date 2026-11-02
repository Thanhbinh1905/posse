package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/store"
)

func (s *Service) evaluateBudgets(ctx context.Context, db *store.DB, project store.Project, cfg config.Config) error {
	tasks, err := db.Tasks(ctx, project.ID, true)
	if err != nil {
		return err
	}
	localDay := time.Now().Format(time.DateOnly)
	for _, task := range tasks {
		plan, _ := task.AdmittedPlan()
		taskLimit := cfg.Budget.TaskTokens
		if plan != nil && plan.Version >= 7 {
			taskLimit = plan.BudgetTokens
		}
		if taskLimit <= 0 && cfg.Budget.DailyTokens <= 0 {
			continue
		}
		kind := task.AgentSessionKind
		if kind == "" {
			if plan != nil {
				kind = plan.Launch.Profile.Kind
			} else if profile, ok := cfg.Profiles[task.Profile]; ok {
				kind = profile.Kind
			}
		}
		sessionID := agentSessionID(kind, task.AgentSession)
		if sessionID == "" {
			continue
		}
		counts := taskUsage(task, cfg)
		if !counts.Known {
			continue
		}
		tokens := counts.Input + counts.Output + counts.CacheRead + counts.CacheWrite
		taskTotal, dailyTotal, err := db.RecordTaskBudgetUsage(ctx, project.ID, task.ID, max(task.Launches, 1), sessionID, kind, localDay, tokens)
		if err != nil {
			return err
		}
		if taskLimit > 0 && taskTotal > taskLimit {
			if err := recordBudgetCrossing(ctx, db, project, task, "task", fmt.Sprint(task.ID), taskTotal, taskLimit); err != nil {
				return err
			}
		}
		if cfg.Budget.DailyTokens > 0 && dailyTotal > cfg.Budget.DailyTokens {
			if err := recordBudgetCrossing(ctx, db, project, task, "daily", localDay, dailyTotal, cfg.Budget.DailyTokens); err != nil {
				return err
			}
		}
	}
	return nil
}

func recordBudgetCrossing(ctx context.Context, db *store.DB, project store.Project, task store.Task, scope, key string, usage, limit int64) error {
	data, err := json.Marshal(map[string]any{"scope": scope, "scope_key": key, "usage_tokens": usage, "limit_tokens": limit})
	if err != nil {
		return err
	}
	summary := fmt.Sprintf("%s exceeded its %s token budget (%d of %d tokens)", taskIDString(task.Seq), scope, usage, limit)
	_, err = db.CreateBudgetCrossingNotice(ctx, project.ID, task.ID, scope, key, summary, string(data))
	return err
}
