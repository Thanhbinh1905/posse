package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanhbinh1905/posse/internal/herdr"
	"github.com/thanhbinh1905/posse/internal/store"
)

func TestReconcileObservationWritesReturnRetryableBusyUnderConcurrentWriter(t *testing.T) {
	db, project, first := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	tasks := []store.Task{first}
	for seq := 2; seq <= 4; seq++ {
		task := store.Task{Seq: seq, Type: "ship", Title: fmt.Sprintf("Task %d", seq), LandingMode: "local", WorktreePath: first.WorktreePath, PaneID: fmt.Sprintf("w1:p%d", seq), PaneLabel: fmt.Sprintf("posse:shop:t%d", seq), HerdrWorkspaceID: "w1", AutonomyReview: "ask", AutonomyLand: "ask"}
		id, err := db.CreateTask(ctx, project.ID, task)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "agent ready"); err != nil {
			t.Fatal(err)
		}
		task.ID, task.ProjectID, task.State = id, project.ID, store.StateWorking
		tasks = append(tasks, task)
	}

	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()

	panes := make([]herdr.Pane, 0, len(tasks))
	for _, task := range tasks {
		panes = append(panes, herdr.Pane{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"})
	}
	writeCtx, cancel := context.WithTimeout(ctx, ReconcileBudget)
	defer cancel()
	started := time.Now()
	_, err = ReconcileSnapshot(writeCtx, db, project.ID, herdr.Snapshot{Panes: panes}, time.Now())
	if !store.IsBusy(err) {
		t.Fatalf("reconcile error = %v, want typed retryable store contention", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconcile still exposed context deadline: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= ReconcileBudget {
		t.Fatalf("reconcile waited %s, want a result within its %s budget", elapsed, ReconcileBudget)
	}
	if got := strings.Count(err.Error(), "update Task t"); got != len(tasks) {
		t.Fatalf("failed observation updates = %d, want all %d Tasks: %v", got, len(tasks), err)
	}
}

func TestReconcileReAdoptsPaneByLabelAndHerdrIdleIsNotDone(t *testing.T) {
	db, project, _ := createWorkingTask(t)
	defer db.Close()
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{
		PaneID: "w2:p9", WorkspaceID: "w2", Label: "posse:shop:t1", Agent: "claude", AgentStatus: "done",
	}}}
	now := time.Now()
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, fake.SnapshotValue, now); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.PaneID != "w2:p9" || updated.HerdrWorkspaceID != "w2" {
		t.Fatalf("Task was not re-adopted by pane label: %#v", updated)
	}
	if updated.State != store.StateWorking {
		t.Fatalf("Herdr idle changed Task state to %q", updated.State)
	}
	if fake.CallCount("session.snapshot") != 0 {
		t.Fatal("ReconcileSnapshot unexpectedly called the adapter")
	}
}

func TestReconcileDoesNotRestoreStalePaneIDsAfterRecovery(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p3", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskLaunch(ctx, task.ID, task.WorktreePath, "w1", "w1:p2", task.PaneLabel, "recovered-agent"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectServerStartedAt(ctx, project.ID, "restored-generation"); err != nil {
		t.Fatal(err)
	}
	stale := herdr.Snapshot{ServerStartedAt: "pre-restart-generation", Panes: []herdr.Pane{
		{PaneID: "w1:p1", WorkspaceID: "w1", Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"},
		{PaneID: "w1:p4", WorkspaceID: "w1", Label: "posse:shop:lead", Agent: "claude", AgentStatus: "idle"},
	}}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, stale, time.Now()); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil || updated.PaneID != "w1:p2" {
		t.Fatalf("stale snapshot replaced the restored Rider pane: %#v, %v", updated, err)
	}
	currentProject, err := db.ProjectByID(ctx, project.ID)
	if err != nil || currentProject.LeadPaneID != "w1:p3" {
		t.Fatalf("stale snapshot replaced the restored Lead pane: %#v, %v", currentProject, err)
	}
}

func TestReconcileSkipsCompareAndSetRacesDuringBlockedMapping(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		initial     store.State
		agentStatus string
	}{
		{name: "blocked", initial: store.StateWorking, agentStatus: "blocked"},
		{name: "unblocked", initial: store.StateBlocked, agentStatus: "idle"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db, project, task := createWorkingTask(t)
			defer db.Close()
			if testCase.initial == store.StateBlocked {
				if err := db.Transition(context.Background(), task.ID, store.StateWorking, store.StateBlocked, "herdr", "test blocked"); err != nil {
					t.Fatal(err)
				}
			}
			trigger := fmt.Sprintf(`CREATE TRIGGER concurrent_state_change AFTER UPDATE OF agent_absent_since ON tasks BEGIN UPDATE tasks SET state='done' WHERE id=%d; END`, task.ID)
			if _, err := db.ExecContext(context.Background(), trigger); err != nil {
				t.Fatal(err)
			}
			snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: testCase.agentStatus}}}
			if _, err := ReconcileSnapshot(context.Background(), db, project.ID, snapshot, time.Now()); err != nil {
				t.Fatalf("lost Task transition race failed reconcile: %v", err)
			}
			updated, err := db.Task(context.Background(), project.ID, "t1")
			if err != nil || updated.State != store.StateDone {
				t.Fatalf("concurrent Task state was overwritten: %#v, %v", updated, err)
			}
			var notices int
			if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='worker_blocked'`, task.ID).Scan(&notices); err != nil || notices != 0 {
				t.Fatalf("lost blocked mapping created a stale Notice: %d, %v", notices, err)
			}
		})
	}
}

func TestReconcileMarksUnrecoverableMissingPaneLost(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	notices, err := ReconcileSnapshot(context.Background(), db, project.ID, herdr.Snapshot{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != store.StateLost || updated.WorktreePath != task.WorktreePath || len(notices) != 1 || notices[0].Kind != "task_lost" {
		t.Fatalf("unrecoverable missing Rider was not marked lost: task=%#v notices=%#v", updated, notices)
	}
}

func TestReconcileWaitsBeforeMarkingMissingGroupedRiderLost(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p2", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskLaunch(ctx, task.ID, task.WorktreePath, "w2", "w2:p1", task.PaneLabel, "test-agent"); err != nil {
		t.Fatal(err)
	}
	project, err := db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	snapshot := herdr.Snapshot{}
	notices, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != store.StateWorking || updated.AgentAbsentSince != now.UnixMilli() || updated.WorktreePath != task.WorktreePath || len(notices) != 0 {
		t.Fatalf("missing grouped Rider did not receive a recovery grace period: task=%#v notices=%#v", updated, notices)
	}
	notices, err = ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(AgentAbsentGrace-time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	updated, err = db.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != store.StateWorking || len(notices) != 0 {
		t.Fatalf("grouped Rider was marked lost before the grace period: task=%#v notices=%#v err=%v", updated, notices, err)
	}
	notices, err = ReconcileSnapshot(ctx, db, project.ID, snapshot, now.Add(AgentAbsentGrace))
	if err != nil {
		t.Fatal(err)
	}
	updated, err = db.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != store.StateLost || updated.WorktreePath != task.WorktreePath || len(notices) != 1 || notices[0].Kind != "task_lost" {
		t.Fatalf("persistently missing grouped Rider was not marked lost: task=%#v notices=%#v err=%v", updated, notices, err)
	}
}

func TestReconcileSkipsSpawningTaskBeforePaneCreation(t *testing.T) {
	db, project, err := createSpawningTask(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, herdr.Snapshot{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	task, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || task.State != store.StateSpawning {
		t.Fatalf("in-progress spawn was reconciled as lost: %#v, %v", task, err)
	}
}

func TestLeadReconcileNeverStoresLivenessInProjectStatus(t *testing.T) {
	db, err := store.OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(context.Background(), "shop", filepath.Join(t.TempDir(), "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(context.Background(), project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, herdr.Snapshot{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	updated, err := db.ProjectByID(context.Background(), project.ID)
	if err != nil || updated.Status != "active" {
		t.Fatalf("Project status after missing Lead pane = %#v, %v", updated, err)
	}
	if err := db.UpdateProjectStatus(context.Background(), project.ID, "missing"); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearLead(context.Background(), project.ID); err != nil {
		t.Fatal(err)
	}
	updated, err = db.ProjectByID(context.Background(), project.ID)
	if err != nil || updated.Status != "missing" {
		t.Fatalf("ClearLead overwrote repository status: %#v, %v", updated, err)
	}
}

func TestReconcileKeepsDoneTaskAndNoticesWorkerExitOnce(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	if err := db.Transition(context.Background(), task.ID, store.StateWorking, store.StateDone, "worker", "committed"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := ReconcileSnapshot(context.Background(), db, project.ID, herdr.Snapshot{}, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || updated.State != store.StateDone {
		t.Fatalf("done Task became lost: %#v, %v", updated, err)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='worker_exited'`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("worker_exited Notice count = %d, %v", count, err)
	}
}

func TestReconcileKeepsLandingTaskAndNoticesWorkerExitOnce(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	if err := db.Transition(context.Background(), task.ID, store.StateWorking, store.StateDone, "worker", "committed"); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(context.Background(), task.ID, store.StateDone, store.StateLanding, "cli", "gate passed"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := ReconcileSnapshot(context.Background(), db, project.ID, herdr.Snapshot{}, time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || updated.State != store.StateLanding {
		t.Fatalf("landing Task became lost: %#v, %v", updated, err)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='worker_exited'`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("worker_exited Notice count = %d, %v", count, err)
	}
}

func TestReconcileDoesNotLoseDoneTaskWhenAgentExitsInsideOpenPane(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	if err := db.Transition(context.Background(), task.ID, store.StateWorking, store.StateDone, "worker", "committed"); err != nil {
		t.Fatal(err)
	}
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel}}}
	now := time.Now()
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, snapshot, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, snapshot, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("reconcile failed after the done Worker's agent exited: %v", err)
	}
	updated, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || updated.State != store.StateDone {
		t.Fatalf("done Task state = %#v, %v", updated, err)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='worker_exited'`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("worker_exited Notice count = %d, %v", count, err)
	}
}

func TestBlockedMappingUsesHerdrTransitions(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	now := time.Now()
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: "w1", Label: task.PaneLabel, Agent: "claude", AgentStatus: "blocked"}}}
	notices, err := ReconcileSnapshot(context.Background(), db, project.ID, snapshot, now)
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := db.Task(context.Background(), project.ID, "t1")
	if updated.State != store.StateBlocked || len(notices) != 1 || notices[0].Kind != "worker_blocked" {
		t.Fatalf("blocked Task = %#v, notices = %#v", updated, notices)
	}
	snapshot.Panes[0].AgentStatus = "idle"
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, snapshot, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	updated, _ = db.Task(context.Background(), project.ID, "t1")
	if updated.State != store.StateWorking {
		t.Fatalf("blocked Worker was not returned to working: %q", updated.State)
	}
}

func TestMissedEventStateIsRecoveredByReconcile(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{
		PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "blocked",
	}}}
	result, err := Run(context.Background(), db, fake, project.ID, 0, 0, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || updated.State != store.StateBlocked {
		t.Fatalf("missed blocked event was not recovered: %#v, %v", updated, err)
	}
	if len(result.Notices) != 1 || result.Notices[0].Kind != "worker_blocked" {
		t.Fatalf("reconcile Notices = %#v", result.Notices)
	}
}

func TestAgentWithRecordedSessionIsLostAfterTwoMinutesWithoutServerRestart(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	session := json.RawMessage(`{"session_id":"s1"}`)
	present := herdr.Snapshot{ServerStartedAt: "server-start-1", Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: "w1", Label: task.PaneLabel, Agent: "claude", AgentSession: session}}}
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, present, start); err != nil {
		t.Fatal(err)
	}
	absent := herdr.Snapshot{ServerStartedAt: "server-start-1", Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: "w1", Label: task.PaneLabel}}}
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, absent, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	updated, _ := db.Task(context.Background(), project.ID, "t1")
	if updated.State != store.StateWorking || updated.AgentAbsentSince != start.Add(time.Second).UnixMilli() || updated.AgentSession == "" || updated.AgentServerStartedAt != "server-start-1" {
		t.Fatalf("absence and last-seen server were not recorded: %#v", updated)
	}
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, absent, start.Add(120*time.Second)); err != nil {
		t.Fatal(err)
	}
	updated, _ = db.Task(context.Background(), project.ID, "t1")
	if updated.State != store.StateWorking {
		t.Fatalf("Worker was marked lost before grace period: %q", updated.State)
	}
	notices, err := ReconcileSnapshot(context.Background(), db, project.ID, absent, start.Add(121*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	updated, _ = db.Task(context.Background(), project.ID, "t1")
	if updated.State != store.StateLost || updated.WorktreePath != task.WorktreePath || len(notices) != 1 || notices[0].Kind != "task_lost" {
		t.Fatalf("dead Worker was not marked lost with task_lost: task=%#v notices=%#v", updated, notices)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='task_lost'`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("task_lost Notice count = %d, %v", count, err)
	}
}

func TestRecordedSessionWaitsWhenHerdrRestartMakesResumePending(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	present := herdr.Snapshot{ServerStartedAt: "server-start-1", Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: "w1", Label: task.PaneLabel, Agent: "claude", AgentSession: json.RawMessage(`{"session_id":"s1"}`)}}}
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, present, start); err != nil {
		t.Fatal(err)
	}
	resuming := herdr.Snapshot{ServerStartedAt: "server-start-2", Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: "w1", Label: task.PaneLabel}}}
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, resuming, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileSnapshot(context.Background(), db, project.ID, resuming, start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(context.Background(), project.ID, "t1")
	if err != nil || updated.State != store.StateWorking || updated.AgentServerStartedAt != "server-start-1" {
		t.Fatalf("pending Herdr resume was marked lost or server observation overwritten: %#v, %v", updated, err)
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notices WHERE task_id=? AND kind='task_lost'`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pending resume created task_lost Notice count = %d, %v", count, err)
	}
}

func createSpawningTask(t *testing.T) (*store.DB, store.Project, error) {
	t.Helper()
	db, err := store.OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		return nil, store.Project{}, err
	}
	project, err := db.CreateProject(context.Background(), "shop", filepath.Join(t.TempDir(), "repo"), "main")
	if err != nil {
		_ = db.Close()
		return nil, store.Project{}, err
	}
	if _, err := db.CreateTask(context.Background(), project.ID, store.Task{Seq: 1, Type: "ship", Title: "Spawning", LandingMode: "local", PaneLabel: "posse:shop:t1"}); err != nil {
		_ = db.Close()
		return nil, store.Project{}, err
	}
	return db, project, nil
}

func TestStallRequiresWorkingHerdrAgentAndUnchangedOutputAndWorktree(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	progress := &fakeProgress{output: "same", worktree: "HEAD\n"}
	if err := db.UpdateProgress(context.Background(), task.ID, digest(progress.output), digest(progress.worktree), start.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
	notices, err := EvaluateStalls(context.Background(), db, fake, project.ID, 5*time.Minute, start.Add(4*time.Minute), progress)
	if err != nil || len(notices) != 0 {
		t.Fatalf("early stall result = %#v, %v", notices, err)
	}
	notices, err = EvaluateStalls(context.Background(), db, fake, project.ID, 5*time.Minute, start.Add(5*time.Minute), progress)
	if err != nil || len(notices) != 1 || notices[0].Kind != "stalled" {
		t.Fatalf("stall result = %#v, %v", notices, err)
	}
	updated, _ := db.Task(context.Background(), project.ID, "t1")
	if updated.State != store.StateStalled {
		t.Fatalf("Task state = %q", updated.State)
	}
}

func TestProgressChangeResetsStallClockAndRunTakesOneSnapshot(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	progress := &fakeProgress{output: "first", worktree: "HEAD\n"}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "codex", AgentStatus: "working"}}}
	if err := db.UpdateProgress(context.Background(), task.ID, digest("old"), digest("HEAD\n"), start.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), db, fake, project.ID, 5*time.Minute, 0, start.Add(4*time.Minute), progress)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Notices) != 0 || fake.CallCount("session.snapshot") != 1 {
		t.Fatalf("Run() notices=%#v snapshotCalls=%d", result.Notices, fake.CallCount("session.snapshot"))
	}
	updated, _ := db.Task(context.Background(), project.ID, "t1")
	if updated.LastProgressAt != start.Add(4*time.Minute).UnixMilli() {
		t.Fatalf("progress timestamp did not reset: %d", updated.LastProgressAt)
	}
}

func TestProgressChangeMovesStalledTaskBackToWorkingAndResetsProgress(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	start := time.Now().Truncate(time.Millisecond)
	progress := &fakeProgress{output: "new output", worktree: "new tree"}
	if err := db.UpdateProgress(ctx, task.ID, digest("old output"), digest("old tree"), start.Add(-10*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, task.ID, store.StateWorking, store.StateStalled, "cli", "no recent progress"); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
	if notices, err := EvaluateStalls(ctx, db, fake, project.ID, 5*time.Minute, start, progress); err != nil || len(notices) != 0 {
		t.Fatalf("resumed progress produced Notices or an error: %#v, %v", notices, err)
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil || updated.State != store.StateWorking {
		t.Fatalf("Task state after progress resumed = %#v, %v", updated, err)
	}
	if updated.LastOutputHash != "" || updated.LastWorktreeHash != "" || updated.LastProgressAt != 0 {
		t.Fatalf("progress baseline was not reset on return to working: %#v", updated)
	}
}

func TestWorkingTaskIdleWithoutSignalCreatesOneWorkerIdleNotice(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	pane := herdr.Pane{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "idle"}
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{pane}}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, start, time.Minute); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(ctx, project.ID, taskID(task.Seq))
	if err != nil || updated.IdleSince != start.UnixMilli() {
		t.Fatalf("idle start was not recorded: %#v, %v", updated, err)
	}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, start.Add(time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, start.Add(2*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].Kind != "worker_idle" || notices[0].Summary != task.Title+" is idle without a Signal" {
		t.Fatalf("idle Notice count/content = %#v", notices)
	}
}

func TestDoneAgentIdleNoticeSurvivesStallEvaluation(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	start := time.Now().Truncate(time.Millisecond)
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "done"}}}
	progress := &fakeProgress{output: "finished turn", worktree: "unchanged"}

	if _, err := Run(ctx, db, fake, project.ID, time.Minute, time.Minute, start, progress); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil || updated.IdleSince != start.UnixMilli() {
		t.Fatalf("done agent idle start was cleared during stall evaluation: %#v, %v", updated, err)
	}
	notices, err := Run(ctx, db, fake, project.ID, time.Minute, time.Minute, start.Add(time.Minute), progress)
	if err != nil || len(notices.Notices) != 1 || notices.Notices[0].Kind != "worker_idle" {
		t.Fatalf("done agent did not produce worker_idle after idle_after: %#v, %v", notices.Notices, err)
	}
}

func TestIngestSanitizesReportedAgentSession(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{
		PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel,
		Agent: "pi", AgentStatus: "idle", AgentSession: json.RawMessage(`{"session_id":"{\"session_id\":\"nested\"}"}`),
	}}}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, fake.SnapshotValue, time.Now()); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil || updated.AgentSession != "" {
		t.Fatalf("invalid Herdr session propagated into the Task: %#v, %v", updated, err)
	}

	fake.SnapshotValue.Panes[0].AgentSession = json.RawMessage(`{"session_id":"session-123"}`)
	if _, err := ReconcileSnapshot(ctx, db, project.ID, fake.SnapshotValue, time.Now()); err != nil {
		t.Fatal(err)
	}
	updated, err = db.Task(ctx, project.ID, "t1")
	if err != nil || updated.AgentSession != "session-123" {
		t.Fatalf("valid Herdr session was not normalized: %#v, %v", updated, err)
	}
}

func TestRelaunchedWorkerGetsIdleNoticeForItsNewLaunch(t *testing.T) {
	ctx := context.Background()
	db, project, task := createWorkingTask(t)
	defer db.Close()
	firstLaunch, err := db.NextTaskLaunch(ctx, task.ID)
	if err != nil || firstLaunch != 1 {
		t.Fatalf("first launch = %d, %v", firstLaunch, err)
	}
	if _, err := db.CreateNotice(ctx, store.Notice{ProjectID: project.ID, TaskID: task.ID, Kind: "worker_idle", Summary: "old launch idle", DataJSON: `{"launch":1}`}); err != nil {
		t.Fatal(err)
	}
	secondLaunch, err := db.NextTaskLaunch(ctx, task.ID)
	if err != nil || secondLaunch != 2 {
		t.Fatalf("second launch = %d, %v", secondLaunch, err)
	}
	start := time.Now().Truncate(time.Millisecond)
	snapshot := herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "idle"}}}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, start, time.Minute); err != nil {
		t.Fatal(err)
	}
	created, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, start.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || created[0].Kind != "worker_idle" || created[0].DataJSON != `{"launch":2}` {
		t.Fatalf("new launch did not receive its own idle Notice: %#v", created)
	}
}

func TestSkippedStallEvaluationResetsClockAndProgressRaceIsIgnored(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	start := time.Now().Truncate(time.Millisecond)
	progress := &fakeProgress{output: "same", worktree: "HEAD\n"}
	if err := db.UpdateProgress(ctx, task.ID, digest(progress.output), digest(progress.worktree), start.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "idle"}}}
	if notices, err := EvaluateStalls(ctx, db, fake, project.ID, 5*time.Minute, start.Add(4*time.Minute), progress); err != nil || len(notices) != 0 {
		t.Fatalf("non-working evaluation = %#v, %v", notices, err)
	}
	updated, _ := db.Task(ctx, project.ID, "t1")
	if updated.LastProgressAt != 0 || updated.LastOutputHash != "" || updated.LastWorktreeHash != "" {
		t.Fatalf("skipped evaluation did not reset the stall clock: %#v", updated)
	}
	fake.SnapshotValue.Panes[0].AgentStatus = "working"
	baseline := start.Add(4*time.Minute + time.Second)
	if notices, err := EvaluateStalls(ctx, db, fake, project.ID, 5*time.Minute, baseline, progress); err != nil || len(notices) != 0 {
		t.Fatalf("first working evaluation after idle = %#v, %v", notices, err)
	}
	if notices, err := EvaluateStalls(ctx, db, fake, project.ID, 5*time.Minute, baseline.Add(4*time.Minute), progress); err != nil || len(notices) != 0 {
		t.Fatalf("stall clock was not reset after idle: %#v, %v", notices, err)
	}
	notices, err := EvaluateStalls(ctx, db, fake, project.ID, 5*time.Minute, baseline.Add(6*time.Minute), progress)
	if err != nil || len(notices) != 1 || notices[0].Kind != "stalled" {
		t.Fatalf("unchanged output stalled after the new window: %#v, %v", notices, err)
	}

	db2, project2, racedTask := createWorkingTask(t)
	defer db2.Close()
	raceStart := time.Now().Add(-10 * time.Minute)
	if err := db2.UpdateProgress(ctx, racedTask.ID, digest(progress.output), digest(progress.worktree), raceStart.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	raceProgress := &fakeProgress{output: progress.output, worktree: progress.worktree, onWorktree: func() error {
		return db2.Transition(ctx, racedTask.ID, store.StateWorking, store.StateDone, "worker", "completed concurrently")
	}}
	fake2 := herdr.NewFake()
	fake2.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: racedTask.PaneID, Label: racedTask.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
	if notices, err := EvaluateStalls(ctx, db2, fake2, project2.ID, 5*time.Minute, time.Now(), raceProgress); err != nil || len(notices) != 0 {
		t.Fatalf("concurrent completion race failed stall evaluation: %#v, %v", notices, err)
	}
	updated, err = db2.Task(ctx, project2.ID, "t1")
	if err != nil || updated.State != store.StateDone {
		t.Fatalf("concurrent Task state was overwritten: %#v, %v", updated, err)
	}
}

type fakeProgress struct {
	output     string
	worktree   string
	onWorktree func() error
}

func (p *fakeProgress) ReadPane(context.Context, herdr.Adapter, string, int) (string, error) {
	return p.output, nil
}

func (p *fakeProgress) WorktreeFingerprint(context.Context, string) (string, error) {
	if p.onWorktree != nil {
		if err := p.onWorktree(); err != nil {
			return "", err
		}
	}
	return p.worktree, nil
}

func TestTaskGenerationStillGetsRestartGraceAfterProjectGenerationWasRecorded(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.UpdateTaskObservation(ctx, task.ID, task.PaneID, task.HerdrWorkspaceID, "", 0, 0, "old-generation"); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "new-generation"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	snapshot := herdr.Snapshot{ServerStartedAt: "new-generation"}
	if _, err := ReconcileSnapshot(ctx, db, project.ID, snapshot, now); err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != store.StateWorking || updated.AgentAbsentSince != now.UnixMilli() || updated.AgentServerStartedAt != "old-generation" {
		t.Fatalf("Task generation did not preserve restart grace and ownership generation: %#v", updated)
	}
}

func TestRunIgnoresPaneThatDisappearsAfterSnapshot(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	if err := db.UpdateProgress(ctx, task.ID, "previous output", "previous worktree", now.Add(-10*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
	fake.Errors["pane.read"] = &herdr.Error{Code: "pane_not_found", Message: "pane w1:p1 not found"}
	if _, err := Run(ctx, db, fake, project.ID, time.Minute, 0, now, nil); err != nil {
		t.Fatalf("snapshot pane disappearing before its read failed reconciliation: %v", err)
	}

	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != store.StateWorking || updated.LastProgressAt != 0 {
		t.Fatalf("vanished pane did not preserve working state and reset progress: %#v", updated)
	}
}

func TestRunWithMissingGenerationPreservesLastObservedProjectAndAgentGenerations(t *testing.T) {
	db, project, task := createWorkingTask(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.UpdateTaskObservation(ctx, task.ID, task.PaneID, task.HerdrWorkspaceID, "", 0, 0, "generation-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "generation-1"); err != nil {
		t.Fatal(err)
	}
	fake := herdr.NewFake()
	fake.SnapshotValue = herdr.Snapshot{Panes: []herdr.Pane{{PaneID: task.PaneID, WorkspaceID: task.HerdrWorkspaceID, Label: task.PaneLabel, Agent: "claude", AgentStatus: "working"}}}
	if _, err := Run(ctx, db, fake, project.ID, 0, 0, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	projectGeneration, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := db.Task(ctx, project.ID, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if projectGeneration != "generation-1" || updated.AgentServerStartedAt != "generation-1" {
		t.Fatalf("missing generation erased last observations: project=%q task=%#v", projectGeneration, updated)
	}
}

func createWorkingTask(t *testing.T) (*store.DB, store.Project, store.Task) {
	t.Helper()
	db, err := store.OpenAt(filepath.Join(t.TempDir(), "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(t.TempDir(), "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	task := store.Task{Seq: 1, Type: "ship", Title: "Fix issue", LandingMode: "local", WorktreePath: filepath.Join(t.TempDir(), "worktree"), PaneID: "w1:p1", PaneLabel: "posse:shop:t1", HerdrWorkspaceID: "w1", AutonomyReview: "ask", AutonomyLand: "ask"}
	id, err := db.CreateTask(ctx, project.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, id, store.StateSpawning, store.StateWorking, "cli", "agent ready"); err != nil {
		t.Fatal(err)
	}
	task.ID = id
	task.ProjectID = project.ID
	task.State = store.StateWorking
	return db, project, task
}
