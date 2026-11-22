package store

import (
	"context"
	"testing"
	"time"
)

func TestIdleSlotTasksRespectsPRReviewReadiness(t *testing.T) {
	for _, test := range []struct {
		name, mode, verdict, verdictMode string
		blocking                         int
		olderVerdict                     string
		wantIdle                         bool
	}{
		{name: "pr", mode: "pr"},
		{name: "greenhouse without verdict", mode: "greenhouse", wantIdle: true},
		{name: "greenhouse pass", mode: "greenhouse", verdict: "clean"},
		{name: "greenhouse blocking verdict", mode: "greenhouse", verdict: "findings", blocking: 1, wantIdle: true},
		{name: "greenhouse older pass and latest blocker", mode: "greenhouse", verdict: "findings", blocking: 1, olderVerdict: "clean", wantIdle: true},
		{name: "greenhouse nonblocking findings", mode: "greenhouse", verdict: "findings"},
		{name: "greenhouse parallel verdict after gate pass", mode: "greenhouse", verdict: "clean", verdictMode: "parallel", olderVerdict: "clean"},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			readyID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Ready", ShortName: "ready", State: StateSpawning, MergeMode: test.mode}, func(int) (bool, error) { return false, nil })
			if err != nil {
				t.Fatal(err)
			}
			queuedID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Queued", ShortName: "queued", State: StateSpawning}, func(int) (bool, error) { return false, nil })
			if err != nil {
				t.Fatal(err)
			}
			prURL, head := "https://github.com/acme/shop/pull/1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='done',landing_mode=?,pr_url=?,gated_sha=?,created_at=1 WHERE id=?`, test.mode, prURL, head, readyID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE transitions SET at=1 WHERE task_id=?`, readyID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='queued' WHERE id=?`, queuedID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,head_sha,state,checks,review,mergeable,observed_at,confirmed_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, project.ID, readyID, prURL, head, "OPEN", `{"state":"SUCCESS"}`, "APPROVED", "MERGEABLE", 1, 1); err != nil {
				t.Fatal(err)
			}
			if test.olderVerdict != "" {
				if _, err := db.ExecContext(ctx, `INSERT INTO verdicts(task_id,repo,provider,protocol,grant_id,head_sha,target_sha,review_base_sha,verdict,blocking,mode,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, readyID, project.Name, GreenhouseReviewProvider, "2", "older-review-grant", head, head, head, test.olderVerdict, 0, "gate", 1); err != nil {
					t.Fatal(err)
				}
			}
			if test.verdict != "" {
				verdictMode := test.verdictMode
				if verdictMode == "" {
					verdictMode = "gate"
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO verdicts(task_id,repo,provider,protocol,grant_id,head_sha,target_sha,review_base_sha,verdict,blocking,mode,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, readyID, project.Name, GreenhouseReviewProvider, "2", "review-grant", head, head, head, test.verdict, test.blocking, verdictMode, 2); err != nil {
					t.Fatal(err)
				}
			}
			idle, waiting, err := db.IdleSlotTasks(ctx, project.ID, time.Second, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(waiting) != 1 || waiting[0].ID != queuedID {
				t.Fatalf("waiting Tasks = %+v, want queued Task %d", waiting, queuedID)
			}
			if gotIdle := len(idle) > 0; gotIdle != test.wantIdle {
				t.Fatalf("idle Tasks=%+v, want idle=%t for %s mode with verdict %q", idle, test.wantIdle, test.mode, test.verdict)
			}
		})
	}
}
