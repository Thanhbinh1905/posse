package store

import (
	"context"
	"testing"
	"time"
)

func TestIdleSlotTasksExcludesReadyPullRequests(t *testing.T) {
	for _, mode := range []string{"pr", "greenhouse"} {
		t.Run(mode, func(t *testing.T) {
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
			readyID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Ready", ShortName: "ready", State: StateSpawning, MergeMode: mode}, func(int) (bool, error) { return false, nil })
			if err != nil {
				t.Fatal(err)
			}
			queuedID, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: "Queued", ShortName: "queued", State: StateSpawning}, func(int) (bool, error) { return false, nil })
			if err != nil {
				t.Fatal(err)
			}
			prURL, head := "https://github.com/acme/shop/pull/1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='done',landing_mode=?,pr_url=?,gated_sha=?,created_at=1 WHERE id=?`, mode, prURL, head, readyID); err != nil {
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
			idle, waiting, err := db.IdleSlotTasks(ctx, project.ID, time.Second, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(waiting) != 1 || waiting[0].ID != queuedID {
				t.Fatalf("waiting Tasks = %+v, want queued Task %d", waiting, queuedID)
			}
			if len(idle) != 0 {
				t.Fatalf("ready-to-merge %s Task holds an idle Pool slot: %+v", mode, idle)
			}
		})
	}
}
