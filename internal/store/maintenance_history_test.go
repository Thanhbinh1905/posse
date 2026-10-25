package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// maintenanceHistoryFixture holds a Project whose torn-down history carries
// every Notice kind that per-command maintenance looks up, beside one live
// Task of each kind that must still be found.
type maintenanceHistoryFixture struct {
	db                 *DB
	project            Project
	mergeReadyNotice   int64
	reviewCapNotice    int64
	workerExitedNotice int64
	merged, spawning   int64
}

func newMaintenanceHistoryFixture(t *testing.T) maintenanceHistoryFixture {
	t.Helper()
	ctx := context.Background()
	db, err := OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	f := maintenanceHistoryFixture{db: db, project: project}
	create := func(seq int, state State, label string) int64 {
		t.Helper()
		id, err := db.CreateTask(ctx, project.ID, Task{Seq: seq, Type: "change", Title: "history", MergeMode: "pr", PaneLabel: label})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET state=?,launches=1 WHERE id=?`, string(state), id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	notice := func(taskID int64, kind string) int64 {
		t.Helper()
		id, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, TaskID: taskID, Kind: kind, Summary: kind, DataJSON: `{"task_launches":1}`})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for seq := 1; seq <= 40; seq++ {
		id := create(seq, StateTornDown, "posse:shop:history")
		for _, kind := range []string{"merge_ready", "review_cap_reached", "worker_exited", "task_done"} {
			notice(id, kind)
		}
	}
	done := create(100, StateDone, "posse:shop:t100")
	f.mergeReadyNotice = notice(done, "merge_ready")
	f.workerExitedNotice = notice(done, "worker_exited")
	f.reviewCapNotice = notice(create(101, StateWorking, ""), "review_cap_reached")
	f.merged = create(102, StateMerged, "posse:shop:t102")
	f.spawning = create(103, StateSpawning, "")
	// A long-running Project has evaluated all of these for Decisions.
	if err := db.AdvanceDecisionNoticeCursor(ctx, project.ID, f.reviewCapNotice); err != nil {
		t.Fatal(err)
	}
	return f
}

func noticeIDs(notices []Notice) []int64 {
	ids := make([]int64, 0, len(notices))
	for _, notice := range notices {
		ids = append(ids, notice.ID)
	}
	return ids
}

// Decision reconciliation runs on every command. Its Notice lookups must
// return only live Tasks' Notices and read them through index ranges, so their
// cost does not grow with torn-down Tasks or evaluated Notices (#486).
func TestDecisionNoticeLookupsSkipProjectHistory(t *testing.T) {
	ctx := context.Background()
	f := newMaintenanceHistoryFixture(t)

	sources, err := f.db.DecisionSourceNotices(ctx, f.project.ID)
	if err != nil || len(sources) != 1 || sources[0].ID != f.mergeReadyNotice {
		t.Fatalf("decision source Notices = %v, %v; want only live merge_ready %d", noticeIDs(sources), err, f.mergeReadyNotice)
	}
	caps, err := f.db.ReviewCapNotices(ctx, f.project.ID)
	if err != nil || len(caps) != 1 || caps[0].ID != f.reviewCapNotice {
		t.Fatalf("Review cap Notices = %v, %v; want only live %d", noticeIDs(caps), err, f.reviewCapNotice)
	}
	exited, err := f.db.UnansweredWorkerExitedNotices(ctx, f.project.ID)
	if err != nil || len(exited) != 1 || exited[0].ID != f.workerExitedNotice {
		t.Fatalf("unanswered worker_exited Notices = %v, %v; want only live %d", noticeIDs(exited), err, f.workerExitedNotice)
	}

	// A new Notice past the cursor is still a decision source.
	fresh, err := f.db.CreateNotice(ctx, Notice{ProjectID: f.project.ID, Kind: "task_done", Summary: "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	sources, err = f.db.DecisionSourceNotices(ctx, f.project.ID)
	if err != nil || len(sources) != 2 || sources[0].ID != f.mergeReadyNotice || sources[1].ID != fresh {
		t.Fatalf("decision source Notices = %v, %v; want [%d %d]", noticeIDs(sources), err, f.mergeReadyNotice, fresh)
	}

	for name, query := range map[string]string{
		"DecisionSourceNotices":         decisionSourceNoticesQuery,
		"ReviewCapNotices":              reviewCapNoticesQuery,
		"UnansweredWorkerExitedNotices": unansweredWorkerExitedNoticesQuery,
	} {
		rows, err := f.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, f.project.ID, f.project.ID, f.project.ID)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN ") || strings.HasSuffix(step, "notices_project_id_idx (project_id=?)") {
				t.Fatalf("%s walks Project history: %q in plan\n%s", name, step, strings.Join(plan, "\n"))
			}
		}
	}
}

func TestMaintenanceTaskLookupsReturnOnlyMatchingTasks(t *testing.T) {
	ctx := context.Background()
	f := newMaintenanceHistoryFixture(t)

	for state, want := range map[State]int64{StateMerged: f.merged, StateSpawning: f.spawning} {
		tasks, err := f.db.TasksInState(ctx, f.project.ID, state)
		if err != nil || len(tasks) != 1 || tasks[0].ID != want {
			t.Fatalf("%s Tasks = %#v, %v; want Task %d", state, tasks, err, want)
		}
	}

	tasks, err := f.db.TasksWithPaneLabels(ctx, f.project.ID, []string{"posse:shop:t102", "posse:other:t1"})
	if err != nil || len(tasks) != 1 || tasks[0].ID != f.merged {
		t.Fatalf("Tasks with live pane labels = %#v, %v; want merged Task %d", tasks, err, f.merged)
	}
	// Torn-down Tasks keep their label; a live pane carrying it still matches.
	tasks, err = f.db.TasksWithPaneLabels(ctx, f.project.ID, []string{"posse:shop:history"})
	if err != nil || len(tasks) != 40 {
		t.Fatalf("Tasks with a torn-down label = %d, %v; want 40", len(tasks), err)
	}
	if tasks, err := f.db.TasksWithPaneLabels(ctx, f.project.ID, nil); err != nil || len(tasks) != 0 {
		t.Fatalf("Tasks without live labels = %#v, %v", tasks, err)
	}

	open, err := f.db.OpenNoticeCount(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	notices, err := f.db.Notices(ctx, f.project.ID, true)
	if err != nil || open != int64(len(notices)) || open == 0 {
		t.Fatalf("open Notice count = %d, open Notices = %d, %v", open, len(notices), err)
	}
}
