package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestCompletedShipRiderBusyRequiresFreshSignal(t *testing.T) {
	for _, state := range []store.State{store.StateDone, store.StateLanding} {
		t.Run(string(state), func(t *testing.T) {
			db, project, task := createWorkingTask(t)
			defer db.Close()
			ctx := context.Background()
			if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "t2 incident: Rider done"); err != nil {
				t.Fatal(err)
			}
			if err := db.SetTaskGatedSHA(ctx, task.ID, "gated-head"); err != nil {
				t.Fatal(err)
			}
			if state == store.StateLanding {
				if err := db.Transition(ctx, task.ID, store.StateDone, state, "cli", "gate passed"); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.UpdateTaskLanding(ctx, task.ID, "https://github.com/acme/shop/pull/249", ""); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			snapshot := herdr.Snapshot{ServerStartedAt: "generation-1", Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Agent: "pi", AgentStatus: "working"}}}
			for _, elapsed := range []time.Duration{0, CompletedRiderBusyGrace - time.Millisecond, CompletedRiderBusyGrace, 2 * CompletedRiderBusyGrace} {
				notices, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(elapsed))
				if err != nil {
					t.Fatal(err)
				}
				current, err := db.Task(ctx, project.ID, "t1")
				if err != nil {
					t.Fatal(err)
				}
				if elapsed < CompletedRiderBusyGrace {
					if current.State != state || len(notices) != 0 {
						t.Fatalf("busy flicker reopened Task: %#v %#v", current, notices)
					}
				} else {
					if current.State != store.StateWorking || current.GatedSHA != "" || current.PRURL != "https://github.com/acme/shop/pull/249" {
						t.Fatalf("busy Task not safely reopened: %#v", current)
					}
				}
			}
			var count int
			var note string
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*),note FROM transitions WHERE task_id=? AND source='watcher'`, task.ID).Scan(&count, &note); err != nil {
				t.Fatal(err)
			}
			if count != 1 || !strings.Contains(note, task.PaneID) || !strings.Contains(note, "since") {
				t.Fatalf("missing transition evidence: count=%d note=%s", count, note)
			}
			notices, err := db.Notices(ctx, project.ID, false)
			if err != nil || len(notices) != 1 || notices[0].Kind != "rider_resumed" {
				t.Fatalf("missing/doubled Lead Notice: %#v %v", notices, err)
			}
			snapshot.Panes[0].AgentStatus = "idle"
			if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(3*CompletedRiderBusyGrace)); err != nil {
				t.Fatal(err)
			}
			current, _ := db.Task(ctx, project.ID, "t1")
			if current.State != store.StateWorking {
				t.Fatal("Herdr idle marked Task done")
			}
			if _, err := db.RecordWorkerSignal(ctx, task, "done", "Rider finished direct follow-up", nil, ""); err != nil {
				t.Fatal(err)
			}
			current, _ = db.Task(ctx, project.ID, "t1")
			if current.State != store.StateDone {
				t.Fatalf("fresh Signal did not complete Task: %#v", current)
			}
		})
	}
}

func TestCompletedRiderBusyEvidenceResets(t *testing.T) {
	for _, reset := range []string{"idle", "done", "blocked", "unknown", "no-agent", "no-pane", "new-pane", "new-server", "new-completion", "posse-send", "scout", "review"} {
		t.Run(reset, func(t *testing.T) {
			db, project, task := createWorkingTask(t)
			defer db.Close()
			ctx := context.Background()
			if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateDone, "worker", "done"); err != nil {
				t.Fatal(err)
			}
			snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "pi", AgentStatus: "working"}}}
			now := time.Now()
			if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now); err != nil {
				t.Fatal(err)
			}
			changed := snapshot
			changed.Panes = append([]herdr.Pane(nil), snapshot.Panes...)
			switch reset {
			case "idle", "done", "blocked", "unknown":
				changed.Panes[0].AgentStatus = reset
			case "no-agent":
				changed.Panes[0].Agent = ""
			case "no-pane":
				changed.Panes = nil
			case "new-pane":
				changed.Panes[0].PaneID = "w2:p1"
			case "new-server":
				changed.ServerStartedAt = "new-generation"
			case "new-completion", "posse-send":
				message, err := db.QueueMessage(ctx, task.ID, "Lead follow-up", false)
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := db.ClaimMessage(ctx, message, "claim", now.UnixMilli(), 0); err != nil || !ok {
					t.Fatalf("claim: %v %v", ok, err)
				}
				if err := db.MarkMessageSubmitting(ctx, message, "claim"); err != nil {
					t.Fatal(err)
				}
				if err := db.MarkClaimedMessageDelivered(ctx, message, "claim", now.UnixMilli()); err != nil {
					t.Fatal(err)
				}
				if reset == "new-completion" {
					if _, err := db.RecordWorkerSignal(ctx, task, "done", "done again", nil, ""); err != nil {
						t.Fatal(err)
					}
				}
			case "scout", "review":
				if _, err := db.ExecContext(ctx, `UPDATE tasks SET type=? WHERE id=?`, reset, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReconcileSnapshot(ctx, db, project.ID, changed, now.Add(CompletedRiderBusyGrace)); err != nil {
				t.Fatal(err)
			}
			current, _ := db.Task(ctx, project.ID, "t1")
			want := store.StateDone
			if reset == "posse-send" {
				want = store.StateWorking
			}
			if current.State != want {
				t.Fatalf("reset %s reused old busy evidence: %#v", reset, current)
			}
			var count int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM transitions WHERE task_id=? AND source='watcher'`, task.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("false watcher transition: %d %v", count, err)
			}
			if reset == "idle" || reset == "done" || reset == "blocked" || reset == "unknown" || reset == "no-agent" || reset == "no-pane" {
				if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(2*CompletedRiderBusyGrace)); err != nil {
					t.Fatal(err)
				}
				current, _ = db.Task(ctx, project.ID, "t1")
				if current.State != store.StateDone {
					t.Fatal("busy evidence survived a non-busy observation")
				}
			}
		})
	}
}
