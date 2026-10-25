package store

import (
	"context"
	"testing"
)

func TestQuotaExhaustionIsClaimedOncePerLaunch(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Quota", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=1 WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	record := QuotaExhaustion{TaskID: taskID, Launch: 1, Agent: "claude", Profile: "opus", Line: "You've hit your session limit · resets 7pm", Resets: "7pm"}
	alive := map[int]bool{100: true}
	isAlive := func(pid int) bool { return alive[pid] }

	if claimed, err := db.ClaimQuotaExhaustion(ctx, QuotaExhaustion{TaskID: taskID, Launch: 2, Agent: "claude", Profile: "opus", Line: record.Line}, 100, isAlive); err != nil || claimed {
		t.Fatalf("claim for a launch that is not current = %t, %v; want refused", claimed, err)
	}
	if claimed, err := db.ClaimQuotaExhaustion(ctx, record, 100, isAlive); err != nil || !claimed {
		t.Fatalf("first claim = %t, %v", claimed, err)
	}
	if claimed, err := db.ClaimQuotaExhaustion(ctx, record, 200, isAlive); err != nil || claimed {
		t.Fatalf("claim while the owner is alive = %t, %v; want refused", claimed, err)
	}
	alive[100] = false
	if claimed, err := db.ClaimQuotaExhaustion(ctx, record, 200, isAlive); err != nil || !claimed {
		t.Fatalf("takeover of a dead owner's claim = %t, %v", claimed, err)
	}
	notice := Notice{ProjectID: project.ID, TaskID: taskID, Kind: "quota_fallback", Summary: "Quota: relaunched with sonnet"}
	if created, err := db.FinishQuotaExhaustion(ctx, taskID, 1, 100, "fallback", "sonnet", "relaunched", notice); err != nil || created != nil {
		t.Fatalf("finish by a replaced owner = %#v, %v; want no change", created, err)
	}
	created, err := db.FinishQuotaExhaustion(ctx, taskID, 1, 200, "fallback", "sonnet", "relaunched", notice)
	if err != nil || created == nil || created.ID == 0 {
		t.Fatalf("finish = %#v, %v", created, err)
	}
	if again, err := db.FinishQuotaExhaustion(ctx, taskID, 1, 200, "decision", "", "again", notice); err != nil || again != nil {
		t.Fatalf("second finish = %#v, %v; want no change", again, err)
	}
	if claimed, err := db.ClaimQuotaExhaustion(ctx, record, 300, isAlive); err != nil || claimed {
		t.Fatalf("claim after an outcome = %t, %v; want refused", claimed, err)
	}
	stored, err := db.TaskQuotaExhaustion(ctx, taskID, 1)
	if err != nil || stored.Status != "fallback" || stored.Fallback != "sonnet" || stored.Resets != "7pm" {
		t.Fatalf("stored record = %#v, %v", stored, err)
	}
	signals, err := db.TaskSignals(ctx, taskID, 10)
	if err != nil {
		t.Fatal(err)
	}
	verbs := map[string]int{}
	for _, signal := range signals {
		verbs[signal.Verb]++
	}
	if verbs["quota_exhausted"] != 1 || verbs["quota_fallback"] != 1 {
		t.Fatalf("audit signals = %#v", verbs)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "quota_fallback" {
		t.Fatalf("notices = %#v, %v", notices, err)
	}
}

func TestQuotaDecisionBecomesObsoleteWhenTheWorkerIsRelaunched(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Quota", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=1 WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	decision, err := db.RaiseDecision(ctx, DecisionRequest{ProjectID: project.ID, TaskID: taskID, Origin: "notice:1", Kind: "quota", Question: "Relaunch or discard?", Options: []string{"relaunch", "discard"}})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v, %v", pending, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET launches=2 WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	pending, err = db.Decisions(ctx, project.ID, true)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after relaunch = %#v, %v; want decision %d obsolete", pending, err, decision.ID)
	}
}
