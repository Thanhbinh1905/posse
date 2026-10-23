package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMergeQueueOrdersByDoneSignalKeepsInFlightHeadAndDequeues(t *testing.T) {
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
	// Task creation order differs from done Signal order.
	ids := map[string]int64{}
	for name, doneAt := range map[string]int64{"late": 3000, "first": 1000, "second": 2000} {
		id, _, err := db.CreateTaskWithSequence(ctx, project.ID, project.Name, Task{Type: "change", Title: name, ShortName: name, State: StateSpawning, MergeMode: "pr"}, func(int) (bool, error) { return false, nil })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='done' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO signals(task_id,verb,at) VALUES(?,'done',?)`, id, doneAt); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	now := time.Now().UnixMilli()
	for _, name := range []string{"late", "first", "second"} {
		if err := db.EnterMergeQueue(ctx, ids[name], "", "main", now); err != nil {
			t.Fatal(err)
		}
	}
	place := func(name string) MergeQueuePlace {
		t.Helper()
		got, err := db.PlaceInMergeQueue(ctx, ids[name], "", "main", time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := place("late"); got.Position != 3 || got.Length != 3 || got.HeadTaskID != ids["first"] {
		t.Fatalf("queue does not follow done Signal order: %+v", got)
	}

	// An in-flight merge-target instruction keeps its Task at the head, also
	// after delivery moves it to working.
	if _, err := db.QueueValidationFixInstruction(ctx, ids["second"], "freshness", strings.Repeat("a", 40), "freshness::"+strings.Repeat("a", 40)+":tip:2000", "Merge origin/main."); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='working' WHERE id=?`, ids["second"]); err != nil {
		t.Fatal(err)
	}
	if got := place("first"); got.Position != 2 || got.HeadTaskID != ids["second"] {
		t.Fatalf("in-flight head was not kept: %+v", got)
	}

	// A Task that leaves done for another reason is dequeued.
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='needs-decision' WHERE id=?`, ids["first"]); err != nil {
		t.Fatal(err)
	}
	if got := place("late"); got.Position != 2 || got.Length != 2 {
		t.Fatalf("Task that left done still holds a place: %+v", got)
	}

	// The next done Signal ends the in-flight instruction; the Task keeps its
	// original done Signal order.
	if _, err := db.ExecContext(ctx, `INSERT INTO signals(task_id,verb,at) VALUES(?,'done',?)`, ids["second"], time.Now().UnixMilli()+1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='done' WHERE id=?`, ids["second"]); err != nil {
		t.Fatal(err)
	}
	if got := place("second"); !got.Head() || got.Length != 2 {
		t.Fatalf("synced Task lost its place: %+v", got)
	}

	// A red head leaves the queue and stays out until its checks turn green,
	// then re-enters at the back.
	if err := db.DequeueMergeQueue(ctx, ids["second"], "", "checks_failed", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if got := place("second"); got.Queued() {
		t.Fatalf("red Task re-entered without a new done Signal or green checks: %+v", got)
	}
	if got := place("late"); !got.Head() || got.Length != 1 {
		t.Fatalf("next entry was not promoted after the red head left: %+v", got)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO pr_observations(project_id,task_id,pr_url,state,checks,observed_at) VALUES(?,?,?,?,?,?)`, project.ID, ids["second"], "https://github.com/acme/shop/pull/2", "OPEN", `{"state":"SUCCESS"}`, time.Now().UnixMilli()+5); err != nil {
		t.Fatal(err)
	}
	if got := place("second"); got.Position != 2 || got.HeadTaskID != ids["late"] {
		t.Fatalf("green Task did not re-enter at the back: %+v", got)
	}
	places, err := db.MergeQueuePlaces(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(places[ids["late"]][0].Position, places[ids["second"]][0].Position, len(places[ids["first"]])); got != "1 2 0" {
		t.Fatalf("read-only queue places = %s", got)
	}
}
