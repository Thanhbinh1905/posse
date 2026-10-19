package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestCompletedChangeWorkerReopensOnlyAfterTurnBoundary(t *testing.T) {
	for _, state := range []store.State{store.StateDone, store.StateMerging} {
		for _, endStatus := range []string{"idle", "done"} {
			t.Run(string(state)+"/"+endStatus, func(t *testing.T) {
				db, project, task := createWorkingTask(t)
				defer db.Close()
				ctx := context.Background()
				if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "t2 incident: Worker done"); err != nil {
					t.Fatal(err)
				}
				if err := db.SetTaskGatedSHA(ctx, task.ID, "gated-head"); err != nil {
					t.Fatal(err)
				}
				if state == store.StateMerging {
					if err := db.Transition(ctx, task.ID, store.StateDone, state, "cli", "gate passed"); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.UpdateTaskMerge(ctx, task.ID, "https://github.com/acme/shop/pull/249", ""); err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				snapshot := herdr.Snapshot{ServerStartedAt: "generation-1", Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Agent: "pi", AgentStatus: "working"}}}
				for _, elapsed := range []time.Duration{0, 10 * time.Second, time.Hour} {
					notices, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(elapsed))
					if err != nil {
						t.Fatal(err)
					}
					current, err := db.Task(ctx, project.ID, "t1")
					if err != nil {
						t.Fatal(err)
					}
					if current.State != state || len(notices) != 0 {
						t.Fatalf("same-turn busy status reopened Task: %#v %#v", current, notices)
					}
				}

				snapshot.Panes[0].AgentStatus = endStatus
				if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(time.Hour+time.Second)); err != nil {
					t.Fatal(err)
				}
				current, _ := db.Task(ctx, project.ID, "t1")
				if current.State != state {
					t.Fatalf("turn end changed completed Task state: %#v", current)
				}

				snapshot.Panes[0].AgentStatus = "working"
				notices, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(time.Hour+2*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				current, err = db.Task(ctx, project.ID, "t1")
				if err != nil || current.State != store.StateWorking || current.GatedSHA != "" || current.PRURL != "https://github.com/acme/shop/pull/249" {
					t.Fatalf("new turn did not safely reopen Task: %#v %v", current, err)
				}
				if len(notices) != 1 || notices[0].Kind != "worker_resumed" {
					t.Fatalf("new turn Notice = %#v; want one worker_resumed", notices)
				}
				var count int
				var note string
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*),note FROM transitions WHERE task_id=? AND source='watcher'`, task.ID).Scan(&count, &note); err != nil {
					t.Fatal(err)
				}
				if count != 1 || !strings.Contains(note, task.PaneID) || !strings.Contains(note, "turn ended") {
					t.Fatalf("missing transition evidence: count=%d note=%s", count, note)
				}
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='worker_resumed'`, task.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("missing/doubled Lead Notice: %d %v", count, err)
				}
			})
		}
	}
}

func TestCompletedWorkerBoundaryMustMatchPaneAndCompletion(t *testing.T) {
	for _, change := range []string{"pane", "server", "completion"} {
		t.Run(change, func(t *testing.T) {
			db, project, task := createWorkingTask(t)
			defer db.Close()
			ctx := context.Background()
			if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			snapshot := herdr.Snapshot{ServerStartedAt: "generation-1", Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "pi", AgentStatus: "working"}}}
			if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now); err != nil {
				t.Fatal(err)
			}
			snapshot.Panes[0].AgentStatus = "idle"
			if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			snapshot.Panes[0].AgentStatus = "working"
			switch change {
			case "pane":
				snapshot.Panes[0].PaneID = "w2:p1"
			case "server":
				snapshot.ServerStartedAt = "generation-2"
			case "completion":
				if err := db.Transition(ctx, task.ID, store.StateDone, store.StateWorking, "lead", "follow-up"); err != nil {
					t.Fatal(err)
				}
				if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "done again"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			current, err := db.Task(ctx, project.ID, "t1")
			if err != nil || current.State != store.StateDone {
				t.Fatalf("old turn boundary reopened changed completion identity: %#v %v", current, err)
			}
		})
	}
}
