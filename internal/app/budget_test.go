package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/config"
	"github.com/thanhbinh1905/posse/internal/dispatch"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
	"github.com/thanhbinh1905/posse/internal/taskplan"
	"github.com/thanhbinh1905/posse/internal/usage"
)

func TestBudgetConfigIsExposedBySchema(t *testing.T) {
	f := newPreferenceSetFixture(t)
	output := runPreferenceSetCLI(t, f.service, 0, "config", "schema")
	for _, key := range []string{"budget.daily_tokens", "budget.task_tokens"} {
		if !strings.Contains(output, key) {
			t.Errorf("config schema omits %s: %s", key, output)
		}
	}
}

func TestBudgetConfigAndBriefOverride(t *testing.T) {
	f := newPreferenceSetFixture(t)
	path := filepath.Join(f.home, "projects", "shop", "config.toml")
	if err := os.WriteFile(path, []byte("[budget]\ndaily_tokens = 4000\ntask_tokens = 2500\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.home, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Budget.DailyTokens != 4000 || cfg.Budget.TaskTokens != 2500 {
		t.Fatalf("budget config = %#v", cfg.Budget)
	}
	for _, key := range []string{"budget.daily_tokens", "budget.task_tokens"} {
		if err := config.ValidateSetting(key, int64(-1), true); err == nil {
			t.Errorf("negative %s was accepted", key)
		}
	}
	brief, err := dispatch.ParseBriefText("---\ntype: change\ntitle: Budgeted Task\ndone_when: usage stays bounded\nbudget_tokens: 1800\n---\n")
	if err != nil || brief.BudgetTokens != 1800 {
		t.Fatalf("brief budget = %d, %v", brief.BudgetTokens, err)
	}
}

func TestTaskBudgetDefaultAndBriefOverrideAreFrozenInPlan(t *testing.T) {
	brief := taskplan.Select("brief", "budget.md", []byte(`---
type: change
title: Budgeted Task
done_when: usage stays bounded
---
`))
	request := taskplan.Request{
		Brief:     brief,
		Execution: taskplan.Execution{Mode: "sandbox", Scope: "repo", Base: "main", Target: "main"},
		Launch:    taskplan.Launch{ProfileName: "worker", Profile: config.Profile{Kind: "pi"}},
		Review:    "none", Delivery: "local", Paths: taskplan.Paths{Scratch: "/scratch", Output: "/output"},
		Pool: config.Pool{Clean: "warm"}, MergeMethod: "squash", TaskBudgetTokens: 900,
	}
	plan, err := taskplan.Resolve(request)
	if err != nil || plan.BudgetTokens != 900 {
		t.Fatalf("configured budget plan = %d, %v", plan.BudgetTokens, err)
	}
	request.Brief = taskplan.Select("brief", "budget.md", []byte(`---
type: change
title: Budgeted Task
done_when: usage stays bounded
budget_tokens: 450
---
`))
	plan, err = taskplan.Resolve(request)
	if err != nil || plan.BudgetTokens != 450 {
		t.Fatalf("Brief override plan = %d, %v", plan.BudgetTokens, err)
	}
}

func TestBudgetCrossingsRaiseOneNoticeAndDecisionPerScopeWithoutStoppingWorker(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sessionRoot := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", sessionRoot)
	sessionID := "budget-test"
	sessionPath := filepath.Join(sessionRoot, "sessions", "session_"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte(`{"type":"message","message":{"role":"assistant","usage":{"input":50,"output":20,"cacheRead":10,"cacheWrite":5}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if counts := usage.Read("pi", sessionID, f.sandbox); !counts.Known || counts.Input+counts.Output+counts.CacheRead+counts.CacheWrite != 85 {
		t.Fatalf("usage reader counts = %#v", counts)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET agent_session=?,agent_session_kind='pi' WHERE project_id=? AND seq=1`, `{"session_id":"`+sessionID+`"}`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[budget]\ndaily_tokens = 70\ntask_tokens = 80\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(f.home, nil)
	t.Chdir(f.repo)
	for i := 0; i < 3; i++ {
		if code, output := runCLI(t, service, "show", "t1"); code != 0 {
			t.Fatalf("user-facing show exit=%d output=%s", code, output)
		}
	}
	notices, err := db.Notices(ctx, f.project.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := db.Decisions(ctx, f.project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 || len(decisions) != 2 {
		t.Fatalf("budget crossing notices=%#v decisions=%#v", notices, decisions)
	}
	hasTaskBudget, hasDailyBudget := false, false
	hasDailySummary := false
	for _, notice := range notices {
		if notice.Kind != "budget_exceeded" {
			t.Fatalf("unexpected Notice kind %q", notice.Kind)
		}
		hasDailySummary = hasDailySummary || strings.Contains(notice.Summary, "Project daily token budget")
	}
	for _, decision := range decisions {
		if decision.Kind != "budget" || len(decision.Options) != 2 || decision.Options[0] != "continue" || decision.Options[1] != "interrupt" {
			t.Fatalf("budget decision = %#v", decision)
		}
		hasTaskBudget = hasTaskBudget || strings.HasPrefix(decision.Origin, "budget:task:")
		hasDailyBudget = hasDailyBudget || strings.HasPrefix(decision.Origin, "budget:daily:")
	}
	if !hasTaskBudget || !hasDailyBudget || !hasDailySummary {
		t.Fatalf("budget crossings by scope: task=%t daily=%t daily_summary=%t", hasTaskBudget, hasDailyBudget, hasDailySummary)
	}
	secondID := "budget-relaunch"
	secondPath := filepath.Join(sessionRoot, "sessions", "session_"+secondID+".jsonl")
	if err := os.WriteFile(secondPath, []byte(`{"type":"message","message":{"role":"assistant","usage":{"input":5,"output":0,"cacheRead":0,"cacheWrite":0}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=2,agent_session=?,agent_session_kind='pi' WHERE project_id=? AND seq=1`, `{"session_id":"`+secondID+`"}`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if code, output := runCLI(t, service, "show", "t1"); code != 0 {
		t.Fatalf("user-facing show after relaunch exit=%d output=%s", code, output)
	}
	var summed int64
	if err := db.QueryRowContext(ctx, `SELECT SUM(tokens) FROM task_budget_usage WHERE task_id=(SELECT id FROM tasks WHERE project_id=? AND seq=1)`, f.project.ID).Scan(&summed); err != nil || summed != 90 {
		t.Fatalf("usage across launches = %d, %v", summed, err)
	}
	task, err := db.Task(ctx, f.project.ID, "t1")
	if err != nil || task.State != store.StateWorking {
		t.Fatalf("budget crossing stopped Worker: task=%#v err=%v", task, err)
	}
}

func TestBudgetDecisionsExpireWithTheirWorkerAndPeriod(t *testing.T) {
	for _, test := range []struct {
		name       string
		origin     func() string
		transition store.State
		launches   bool
	}{
		{name: "worker relaunch", origin: func() string { return "budget:task:1" }, launches: true},
		{name: "task completion", origin: func() string { return "budget:task:1" }, transition: store.StateDone},
		{name: "daily period rollover", origin: func() string { return "budget:daily:" + time.Now().AddDate(0, 0, -1).Format(time.DateOnly) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateWorking)
			defer f.db.Close()
			ctx := context.Background()
			decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{
				ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "budget", Origin: test.origin(),
				Question: "Continue or interrupt?", Options: []string{"continue", "interrupt"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.launches {
				if _, err := f.db.ExecContext(ctx, `UPDATE tasks SET launches=launches+1 WHERE id=?`, f.task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.transition != "" {
				if err := f.db.Transition(ctx, f.task.ID, store.StateWorking, test.transition, "worker", "finished"); err != nil {
					t.Fatal(err)
				}
			}
			decisions, err := f.db.Decisions(ctx, f.project.ID, true)
			if err != nil || len(decisions) != 0 {
				t.Fatalf("stale budget Decision remained pending: %#v, %v", decisions, err)
			}
			stored, err := f.db.Decision(ctx, f.project.ID, decision.ID)
			if err != nil || stored.ObsoleteAt == 0 {
				t.Fatalf("stale budget Decision was not obsoleted: %#v, %v", stored, err)
			}
			if _, err := f.db.AnswerDecision(ctx, f.project.ID, decision.ID, "interrupt", "User chose interrupt"); err == nil {
				t.Fatal("stale budget Decision accepted an answer")
			}
		})
	}
}

func TestBudgetDecisionAnswersApplyWithoutStoppingTheWorker(t *testing.T) {
	for _, option := range []string{"continue", "interrupt"} {
		t.Run(option, func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateWorking)
			defer f.db.Close()
			ctx := context.Background()
			decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{
				ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "budget", Origin: "budget:task:1",
				Question: "Continue or interrupt?", Options: []string{"continue", "interrupt"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if code, output, stderr := f.run("decide", fmt.Sprint(decision.ID), option, "--user-approved", "User chose "+option); code != 0 {
				t.Fatalf("decide %s: %s %s", option, output, stderr)
			}
			fake := f.service.Herdr.(*herdr.Fake)
			fake.Results["pane.read"] = []byte(`{"text":"worker output"}`)
			if option == "interrupt" {
				for i := range fake.SnapshotValue.Panes {
					if fake.SnapshotValue.Panes[i].PaneID == f.task.PaneID {
						fake.SnapshotValue.Panes[i].AgentStatus = "working"
					}
				}
				fake.BeforeCall = func(method string) {
					if method == "agent.send_keys" {
						for i := range fake.SnapshotValue.Panes {
							if fake.SnapshotValue.Panes[i].PaneID == f.task.PaneID {
								fake.SnapshotValue.Panes[i].AgentStatus = "idle"
							}
						}
					}
				}
			}
			code, output, stderr := f.run("apply", fmt.Sprint(decision.ID))
			if code != 0 {
				t.Fatalf("apply %s: %s %s", option, output, stderr)
			}
			task, err := f.db.Task(ctx, f.project.ID, "t1")
			if err != nil || task.State != store.StateWorking {
				t.Fatalf("%s changed Worker state: task=%#v err=%v", option, task, err)
			}
			for _, call := range fake.Calls {
				if call.Method == "workspace.close" || call.Method == "pane.close" || call.Method == "worktree.remove" {
					t.Fatalf("budget %s tore down Worker: %#v", option, fake.Calls)
				}
			}
			if option == "continue" {
				if fake.CallCount("agent.send_keys") != 0 {
					t.Fatalf("continue sent abort keys: %#v", fake.Calls)
				}
				return
			}
			cfg, err := config.Load(f.home, f.project.Name)
			if err != nil {
				t.Fatal(err)
			}
			var sentKeys []string
			for _, call := range fake.Calls {
				if call.Method == "agent.send_keys" {
					sentKeys, _ = call.Params["keys"].([]string)
				}
			}
			if fake.CallCount("agent.send_keys") != 1 || fmt.Sprint(sentKeys) != fmt.Sprint(cfg.Kinds["claude"].InterruptKeys) {
				t.Fatalf("interrupt keys=%v calls=%#v, want %v", sentKeys, fake.Calls, cfg.Kinds["claude"].InterruptKeys)
			}
			messages, err := f.db.TaskMessagesAfter(ctx, f.task.ID, 0, 10)
			if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Body, "Stop work now") || !strings.Contains(messages[0].Body, "posse signal done") {
				t.Fatalf("interrupt stop instruction = %#v, %v", messages, err)
			}
		})
	}
}

func TestStaleBudgetInterruptCannotApplyAfterItsScopeChanges(t *testing.T) {
	for _, test := range []struct {
		name  string
		stale func(*testing.T, *prMergeFixture, int64)
	}{
		{name: "worker relaunch", stale: func(t *testing.T, f *prMergeFixture, _ int64) {
			if _, err := f.db.ExecContext(context.Background(), `UPDATE tasks SET launches=launches+1 WHERE id=?`, f.task.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "task completion", stale: func(t *testing.T, f *prMergeFixture, _ int64) {
			if err := f.db.Transition(context.Background(), f.task.ID, store.StateWorking, store.StateDone, "worker", "finished"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "daily rollover", stale: func(t *testing.T, f *prMergeFixture, decisionID int64) {
			day := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)
			if _, err := f.db.ExecContext(context.Background(), `UPDATE decisions SET origin=? WHERE id=?`, "budget:daily:"+day, decisionID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPRMergeFixture(t, "local", store.StateWorking)
			defer f.db.Close()
			ctx := context.Background()
			decision, err := f.db.RaiseDecision(ctx, store.DecisionRequest{
				ProjectID: f.project.ID, TaskID: f.task.ID, Kind: "budget", Origin: "budget:task:1",
				Question: "Continue or interrupt?", Options: []string{"continue", "interrupt"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.AnswerDecision(ctx, f.project.ID, decision.ID, "interrupt", "User chose interrupt"); err != nil {
				t.Fatal(err)
			}
			test.stale(t, f, decision.ID)
			fake := f.service.Herdr.(*herdr.Fake)
			fake.Results["pane.read"] = []byte(`{"text":"worker output"}`)
			for i := range fake.SnapshotValue.Panes {
				if fake.SnapshotValue.Panes[i].PaneID == f.task.PaneID {
					fake.SnapshotValue.Panes[i].AgentStatus = "working"
				}
			}
			code, output, stderr := f.run("apply", fmt.Sprint(decision.ID))
			if code == 0 || !strings.Contains(output+stderr, "decision_refused") {
				t.Fatalf("stale interrupt applied: exit=%d output=%s stderr=%s", code, output, stderr)
			}
			if fake.CallCount("agent.send_keys") != 0 {
				t.Fatalf("stale interrupt sent abort keys: %#v", fake.Calls)
			}
		})
	}
}

func TestUnknownUsageDoesNotTriggerTaskBudget(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET agent_session='missing-session',agent_session_kind='codex' WHERE project_id=? AND seq=1`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "projects", "shop", "config.toml"), []byte("[budget]\ntask_tokens = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.repo)
	service := testService(f.home, nil)
	code, output := runCLI(t, service, "show", "t1")
	if code != 0 || !strings.Contains(output, "known: false") {
		t.Fatalf("unknown usage not shown by user-facing show: exit=%d output=%s", code, output)
	}
	notices, err := db.Notices(ctx, f.project.ID, true)
	if err != nil || len(notices) != 0 {
		t.Fatalf("unknown usage raised budget Notice: notices=%#v err=%v", notices, err)
	}
}

func TestBudgetUsageSurvivesDatabaseRebuild(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.RecordTaskBudgetUsage(ctx, f.project.ID, task.ID, 1, "session", "pi", time.Now().Format(time.DateOnly), 123); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, f.home); err != nil {
		t.Fatal(err)
	}
	var tokens int64
	if err := db.QueryRowContext(ctx, `SELECT SUM(tokens) FROM task_budget_usage WHERE task_id=?`, task.ID).Scan(&tokens); err != nil || tokens != 123 {
		t.Fatalf("budget usage after rebuild = %d, %v", tokens, err)
	}
}

func TestTaskDailyBudgetUsageResetsAtHostLocalDate(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	db, err := store.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.Task(ctx, f.project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format(time.DateOnly)
	taskTotal, dailyTotal, err := db.RecordTaskBudgetUsage(ctx, f.project.ID, task.ID, 1, "session", "pi", today, 60)
	if err != nil || taskTotal != 60 || dailyTotal != 60 {
		t.Fatalf("initial usage totals task=%d daily=%d err=%v", taskTotal, dailyTotal, err)
	}
	taskTotal, dailyTotal, err = db.RecordTaskBudgetUsage(ctx, f.project.ID, task.ID, 1, "session", "pi", today, 75)
	if err != nil || taskTotal != 75 || dailyTotal != 75 {
		t.Fatalf("same-day usage totals task=%d daily=%d err=%v", taskTotal, dailyTotal, err)
	}
	tomorrow := time.Now().AddDate(0, 0, 1).Format(time.DateOnly)
	taskTotal, dailyTotal, err = db.RecordTaskBudgetUsage(ctx, f.project.ID, task.ID, 1, "session", "pi", tomorrow, 80)
	if err != nil || taskTotal != 80 || dailyTotal != 0 {
		t.Fatalf("new-day usage totals task=%d daily=%d err=%v", taskTotal, dailyTotal, err)
	}
}
