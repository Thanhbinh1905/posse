package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestProjectParksOnlyAfterMissingLeadGraceWithNoActiveWorkers(t *testing.T) {
	started := time.UnixMilli(1_000)
	project := store.Project{LeadAbsentSince: started.UnixMilli()}
	grace := time.Hour

	for _, test := range []struct {
		name       string
		now        time.Time
		tasks      []store.Task
		wantParked bool
	}{
		{name: "during grace", now: started.Add(grace - time.Millisecond)},
		{name: "active Worker", now: started.Add(grace), tasks: []store.Task{{State: store.StateWorking}}},
		{name: "grace elapsed", now: started.Add(grace), tasks: []store.Task{{State: store.StateDone}}, wantParked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := projectReadyToPark(project, test.tasks, test.now, grace); got != test.wantParked {
				t.Fatalf("projectReadyToPark() = %t, want %t", got, test.wantParked)
			}
		})
	}
}

func TestNoticeDeliverySkipsMissingHerdrWorkspace(t *testing.T) {
	fixture := newStartFixture(t)
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByID(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if project.HerdrWorkspaceID == "" {
		t.Fatal("fixture Project has no Herdr workspace")
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "Saved work is ready"}); err != nil {
		t.Fatal(err)
	}
	fixture.fake.SnapshotValue = herdr.Snapshot{Workspaces: []herdr.Workspace{{WorkspaceID: "replacement"}}}
	if err := fixture.service.deliverNoticesWithSnapshot(ctx, db, project, &fixture.fake.SnapshotValue); err != nil {
		t.Fatalf("deliver Notice to missing workspace: %v", err)
	}
	if fixture.fake.CallCount("notification.show") != 0 || fixture.fake.CallCount("workspace.report_metadata") != 0 {
		t.Fatalf("Notice delivery attempted calls to missing workspace: %#v", fixture.fake.Calls)
	}
}

func TestNoticeDeliverySkipsSuccessfullyFetchedEmptyWorkspaceSnapshot(t *testing.T) {
	fixture := newStartFixture(t)
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.ProjectByID(ctx, fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, Kind: "task_done", Summary: "Saved work is ready"}); err != nil {
		t.Fatal(err)
	}
	fixture.fake.SnapshotValue = herdr.Snapshot{}
	for _, test := range []struct {
		name  string
		known bool
		wants int
	}{
		{name: "fetch omitted snapshot", wants: 1},
		{name: "use supplied empty snapshot", known: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			beforeSnapshots := fixture.fake.CallCount("session.snapshot")
			var err error
			if test.known {
				empty := herdr.Snapshot{}
				err = fixture.service.deliverNoticesWithSnapshot(ctx, db, project, &empty)
			} else {
				err = fixture.service.deliverNotices(ctx, db, project)
			}
			if err != nil {
				t.Fatalf("deliver Notice with empty Herdr snapshot: %v", err)
			}
			if got := fixture.fake.CallCount("session.snapshot") - beforeSnapshots; got != test.wants {
				t.Fatalf("session.snapshot calls = %d, want %d", got, test.wants)
			}
			if fixture.fake.CallCount("notification.show") != 0 || fixture.fake.CallCount("workspace.report_metadata") != 0 {
				t.Fatalf("Notice delivery attempted calls with no recorded workspace: %#v", fixture.fake.Calls)
			}
		})
	}
}

func TestWatchParksAbandonedProjectWithoutChangingTasksOrNotices(t *testing.T) {
	fixture := newStartFixture(t)
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	taskID, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Saved work", State: store.StateSpawning, PaneID: "w1:p2", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateWorking, store.StateDone, "worker", "completed"); err != nil {
		t.Fatal(err)
	}
	noticeID, err := db.CreateNotice(ctx, store.Notice{ProjectID: fixture.project.ID, TaskID: taskID, Kind: "task_done", Summary: "Saved work is ready"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE projects SET lead_absent_since=? WHERE id=?`, time.Now().Add(-projectParkGrace-time.Minute).UnixMilli(), fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	fixture.fake.SnapshotValue = herdr.Snapshot{Workspaces: []herdr.Workspace{{WorkspaceID: "w1"}}, Panes: []herdr.Pane{{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "idle"}}}
	oldInterval := watchPassInterval
	watchPassInterval = time.Millisecond
	t.Cleanup(func() { watchPassInterval = oldInterval })
	output := &bytes.Buffer{}
	if err := fixture.service.runWatchLoop(&axi.Context{Context: ctx, Out: output, ErrOut: output}, db, fixture.project, 3*time.Millisecond, nil, nil); err != nil {
		t.Fatalf("run Project Watch: %v\n%s", err, output.String())
	}
	parked, err := db.ProjectByID(ctx, fixture.project.ID)
	if err != nil || !parked.IsParked() {
		t.Fatalf("Project after Watch = %+v, %v; want parked", parked, err)
	}
	task, err := db.Task(ctx, fixture.project.ID, "t1")
	if err != nil || task.State != store.StateDone {
		t.Fatalf("saved Task = %+v, %v; want unchanged done Task", task, err)
	}
	notices, err := db.Notices(ctx, fixture.project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].ID != noticeID {
		t.Fatalf("Notices = %+v, %v; want unchanged Notice", notices, err)
	}
	if fixture.fake.CallCount("notification.show") != 0 {
		t.Fatalf("parked Project attempted Notice alert: %#v", fixture.fake.Calls)
	}
	for _, args := range [][]string{{"list", "--all"}, {}} {
		code, output := runCLI(t, fixture.service, args...)
		if code != 0 || !strings.Contains(output, "parked") || !strings.Contains(output, projectParkReason()) {
			t.Fatalf("posse %v = %d: %s", args, code, output)
		}
	}
}

func TestWatchDoesNotParkWhenWorkerIsWorking(t *testing.T) {
	fixture := newStartFixture(t)
	ctx := context.Background()
	db, err := store.Open(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	taskID, err := db.CreateTask(ctx, fixture.project.ID, store.Task{Seq: 1, Type: "change", Title: "Active work", State: store.StateSpawning, PaneID: "w1:p2", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, store.StateSpawning, store.StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE projects SET lead_absent_since=? WHERE id=?`, time.Now().Add(-projectParkGrace-time.Minute).UnixMilli(), fixture.project.ID); err != nil {
		t.Fatal(err)
	}
	fixture.fake.SnapshotValue = herdr.Snapshot{Workspaces: []herdr.Workspace{{WorkspaceID: "w1"}}, Panes: []herdr.Pane{{PaneID: "w1:p2", WorkspaceID: "w1", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "working"}}}
	oldInterval := watchPassInterval
	watchPassInterval = time.Millisecond
	t.Cleanup(func() { watchPassInterval = oldInterval })
	if err := fixture.service.runWatchLoop(&axi.Context{Context: ctx, Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}, db, fixture.project, 3*time.Millisecond, nil, nil); err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByID(ctx, fixture.project.ID)
	if err != nil || project.IsParked() {
		t.Fatalf("Project with a working Worker = %+v, %v; want active", project, err)
	}
}
