package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestWatchRuntimeHeartbeatAndFailureNoticeEpisode(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "watch", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	watch := WatchRuntime{PID: 123, Token: "episode-one", StartedAt: 100, HeartbeatAt: 101, ExecutablePath: "/usr/bin/posse", ExecutableDevice: 7, ExecutableInode: 11}
	if err := db.SetWatchRuntime(ctx, project.ID, watch); err != nil {
		t.Fatal(err)
	}
	got, err := db.WatchRuntime(ctx, project.ID)
	if err != nil || got != watch {
		t.Fatalf("Watch runtime = %#v, want %#v: %v", got, watch, err)
	}
	raised, err := db.RaiseWatchRuntimeFailure(ctx, project.ID, watch.Token, "Watch runtime heartbeat is stale", 102)
	if err != nil || !raised {
		t.Fatalf("first stale episode notice = %v, %v", raised, err)
	}
	raised, err = db.RaiseWatchRuntimeFailure(ctx, project.ID, watch.Token, "duplicate stale watch", 103)
	if err != nil || raised {
		t.Fatalf("duplicate stale episode notice = %v, %v", raised, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "pr_watch_failing" || notices[0].Summary != "Watch runtime heartbeat is stale" {
		t.Fatalf("stale episode Notices = %#v, %v", notices, err)
	}
	if ok, err := db.HeartbeatWatchRuntime(ctx, project.ID, watch.PID, watch.Token, 104); err != nil || !ok {
		t.Fatalf("recovery heartbeat = %v, %v", ok, err)
	}
	if raised, err := db.RaiseWatchRuntimeFailure(ctx, project.ID, watch.Token, "second stale episode", 105); err != nil || !raised {
		t.Fatalf("second stale episode notice = %v, %v", raised, err)
	}
	if err := db.ClearWatchRuntime(ctx, project.ID, watch.PID, watch.Token); err != nil {
		t.Fatal(err)
	}
	if got, err := db.WatchRuntime(ctx, project.ID); err != nil || got != (WatchRuntime{}) {
		t.Fatalf("cleared Watch runtime = %#v, %v", got, err)
	}
}

func TestRememberProjectServerStartedAtFillsGenerationAlongsideWatchRecovery(t *testing.T) {
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
	watch := WatchRecovery{PaneID: "w1:p2", RetryAt: 123456, FailedStarts: 2, NoticeRaised: true}
	if err := db.SetWatchRecovery(ctx, project.ID, watch); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "server-1"); err != nil {
		t.Fatal(err)
	}
	generation, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "server-1" {
		t.Fatalf("remembered Project generation = %q, want server-1: %v", generation, err)
	}
	gotWatch, err := db.WatchRecovery(ctx, project.ID)
	if err != nil || gotWatch != watch {
		t.Fatalf("remembering Project generation changed Watch recovery: %#v, want %#v: %v", gotWatch, watch, err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "server-2"); err != nil {
		t.Fatal(err)
	}
	generation, err = db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "server-1" {
		t.Fatalf("remembering a later Project generation = %q, want the original server-1: %v", generation, err)
	}
}

func TestRememberKnownGenerationDoesNotContendWithWriters(t *testing.T) {
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
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "server-1"); err != nil {
		t.Fatal(err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()

	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := db.RememberProjectServerStartedAt(readCtx, project.ID, "server-2"); err != nil {
		t.Fatalf("remembering a known generation unnecessarily waited for a write lock: %v", err)
	}
	generation, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "server-1" {
		t.Fatalf("known generation changed: %q, %v", generation, err)
	}
}

func TestStaleProjectObservationCannotRevertRecoveredLead(t *testing.T) {
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
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	stale, err := db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectWorkspace(ctx, project.ID, "w2"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w2", "w2:p1", "posse:shop:lead"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateProjectObservation(ctx, stale, stale.LeadPaneID, stale.HerdrWorkspaceID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	current, err := db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.HerdrWorkspaceID != "w2" || current.LeadPaneID != "w2:p1" || current.LeadLabel != "posse:shop:lead" {
		t.Fatalf("stale Lead observation reverted the recovered identity: %#v", current)
	}
}

func TestUnchangedProjectObservationDoesNotWaitForWriter(t *testing.T) {
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
	if err := db.UpdateProjectObservation(ctx, project, "w1:p1", "w1", 0); err != nil {
		t.Fatal(err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET status='missing' WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := db.UpdateProjectObservation(writeCtx, project, "w1:p1", "w1", 0); err != nil {
		t.Fatalf("unchanged Project observation waited for a write lock: %v", err)
	}
}

func TestUnchangedTaskObservationDoesNotWaitForWriter(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Observation"})
	if err != nil {
		t.Fatal(err)
	}
	observation := struct {
		paneID, workspaceID, agentSession, agentSessionKind string
		absentSince, idleSince                              int64
		serverStartedAt                                     string
	}{"w1:p1", "w1", "session-1", "claude", 10, 20, "server-1"}
	if err := db.UpdateTaskObservation(ctx, taskID, TaskObservation{PaneID: observation.paneID, WorkspaceID: observation.workspaceID, AgentSession: observation.agentSession, AgentSessionKind: observation.agentSessionKind, AgentAbsentSince: observation.absentSince, IdleSince: observation.idleSince, AgentServerStartedAt: observation.serverStartedAt}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET updated_at=1 WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET status='missing' WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := db.UpdateTaskObservation(writeCtx, taskID, TaskObservation{PaneID: observation.paneID, WorkspaceID: observation.workspaceID, AgentSession: observation.agentSession, AgentSessionKind: observation.agentSessionKind, AgentAbsentSince: observation.absentSince, IdleSince: observation.idleSince, AgentServerStartedAt: observation.serverStartedAt}); err != nil {
		t.Fatalf("unchanged Task observation waited for a write lock: %v", err)
	}
	var updatedAt int64
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM tasks WHERE id=?`, taskID).Scan(&updatedAt); err != nil || updatedAt != 1 {
		t.Fatalf("unchanged Task observation changed updated_at to %d: %v", updatedAt, err)
	}
	snapshotData, err := os.ReadFile(db.TaskSnapshotPath(project.Name, 1))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot TaskSnapshot
	if _, err := toml.Decode(string(snapshotData), &snapshot); err != nil || snapshot.Task.UpdatedAt != 1 {
		t.Fatalf("unchanged Task observation did not refresh the recovery snapshot: updated_at=%d error=%v", snapshot.Task.UpdatedAt, err)
	}
}

func TestChangedTaskObservationIsPersisted(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Observation"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET updated_at=1 WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskObservation(ctx, taskID, TaskObservation{PaneID: "w2:p3", WorkspaceID: "w2", AgentSession: "session-2", AgentSessionKind: "claude", AgentAbsentSince: 30, IdleSince: 40, AgentServerStartedAt: "server-2"}); err != nil {
		t.Fatal(err)
	}
	var paneID, workspaceID, agentSession, agentSessionKind, serverStartedAt string
	var absentSince, idleSince, updatedAt int64
	if err := db.QueryRowContext(ctx, `SELECT pane_id,herdr_workspace_id,agent_session,agent_session_kind,agent_absent_since,idle_since,agent_server_started_at,updated_at FROM tasks WHERE id=?`, taskID).Scan(
		&paneID, &workspaceID, &agentSession, &agentSessionKind, &absentSince, &idleSince, &serverStartedAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if paneID != "w2:p3" || workspaceID != "w2" || agentSession != "session-2" || agentSessionKind != "claude" || absentSince != 30 || idleSince != 40 || serverStartedAt != "server-2" || updatedAt <= 1 {
		t.Fatalf("changed Task observation was not persisted: pane=%q workspace=%q session=%q kind=%q absent=%d idle=%d server=%q updated_at=%d", paneID, workspaceID, agentSession, agentSessionKind, absentSince, idleSince, serverStartedAt, updatedAt)
	}
}

func TestChangedProjectObservationIsPersisted(t *testing.T) {
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
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "Lead:shop"); err != nil {
		t.Fatal(err)
	}
	project, err = db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateProjectObservation(ctx, project, "w2:p2", "w2", 123); err != nil {
		t.Fatal(err)
	}
	updated, err := db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LeadPaneID != "w2:p2" || updated.HerdrWorkspaceID != "w2" || updated.LeadAbsentSince != 123 {
		t.Fatalf("changed Project observation was not persisted: %#v", updated)
	}
}

func TestTaskObservationConditionalUpdatePreservesConcurrentObservation(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	writer, err := OpenAt(filepath.Join(home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Observation"})
	if err != nil {
		t.Fatal(err)
	}
	observation := struct {
		paneID, workspaceID, agentSession, agentSessionKind string
		absentSince, idleSince                              int64
		serverStartedAt                                     string
	}{"w2:p3", "w2", "session-2", "claude", 30, 40, "server-2"}
	unchanged, err := db.taskObservationMatches(ctx, taskID, TaskObservation{PaneID: observation.paneID, WorkspaceID: observation.workspaceID, AgentSession: observation.agentSession, AgentSessionKind: observation.agentSessionKind, AgentAbsentSince: observation.absentSince, IdleSince: observation.idleSince, AgentServerStartedAt: observation.serverStartedAt})
	if err != nil || unchanged {
		t.Fatalf("initial Task observation check = unchanged:%t error:%v, want changed", unchanged, err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE tasks SET pane_id=?,herdr_workspace_id=?,agent_session=?,agent_session_kind=?,agent_absent_since=?,idle_since=?,agent_server_started_at=?,updated_at=100 WHERE id=?`,
		observation.paneID, observation.workspaceID, observation.agentSession, observation.agentSessionKind, observation.absentSince, observation.idleSince, observation.serverStartedAt, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.writeTaskObservation(ctx, taskID, TaskObservation{PaneID: observation.paneID, WorkspaceID: observation.workspaceID, AgentSession: observation.agentSession, AgentSessionKind: observation.agentSessionKind, AgentAbsentSince: observation.absentSince, IdleSince: observation.idleSince, AgentServerStartedAt: observation.serverStartedAt}, 200); err != nil {
		t.Fatal(err)
	}
	var updatedAt int64
	if err := db.QueryRowContext(ctx, `SELECT updated_at FROM tasks WHERE id=?`, taskID).Scan(&updatedAt); err != nil || updatedAt != 100 {
		t.Fatalf("conditional Task update overwrote the concurrent observation: updated_at=%d error=%v", updatedAt, err)
	}
}

func TestProjectObservationConditionalUpdatePreservesConcurrentLead(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	writer, err := OpenAt(filepath.Join(home, "posse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectLead(ctx, project.ID, "w1", "w1:p1", "Lead:shop"); err != nil {
		t.Fatal(err)
	}
	stale, err := db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	const paneID, workspaceID = "w2:p2", "w2"
	const absentSince = int64(123)
	unchanged, err := db.projectObservationMatches(ctx, project.ID, paneID, workspaceID, absentSince)
	if err != nil || unchanged {
		t.Fatalf("initial Project observation check = unchanged:%t error:%v, want changed", unchanged, err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE projects SET lead_pane_id=?,herdr_workspace_id=?,lead_absent_since=? WHERE id=?`, "w3:p3", "w3", 456, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.writeProjectObservation(ctx, stale, paneID, workspaceID, absentSince); err != nil {
		t.Fatal(err)
	}
	updated, err := db.ProjectByID(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LeadPaneID != "w3:p3" || updated.HerdrWorkspaceID != "w3" || updated.LeadAbsentSince != 456 {
		t.Fatalf("conditional Project update overwrote the concurrent Lead observation: %#v", updated)
	}
}

func TestWatchRecoveryPersistsAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	want := WatchRecovery{PaneID: "w1:p2", RetryAt: 123456, FailedStarts: 4, NoticeRaised: true}
	if err := db.SetWatchRecovery(ctx, project.ID, want); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.WatchRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened Watch recovery = %#v, want %#v", got, want)
	}
	if err := db.ResetWatchRecovery(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	got, err = db.WatchRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != (WatchRecovery{}) {
		t.Fatalf("reset Watch recovery = %#v, want empty state", got)
	}
}

func TestObserveProjectLeadAgentWritesOnlyChanges(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ observed, want LeadAgent }{
		{LeadAgent{Kind: "claude", Session: "s1"}, LeadAgent{Kind: "claude", Session: "s1"}},
		{LeadAgent{Kind: "claude"}, LeadAgent{Kind: "claude", Session: "s1"}},
		{LeadAgent{Kind: "claude", Session: "s2"}, LeadAgent{Kind: "claude", Session: "s2"}},
		{LeadAgent{Kind: "pi"}, LeadAgent{Kind: "pi"}},
	} {
		if err := db.ObserveProjectLeadAgent(ctx, project.ID, step.observed); err != nil {
			t.Fatal(err)
		}
		if got, err := db.ProjectLeadAgent(ctx, project.ID); err != nil || got != step.want {
			t.Fatalf("after observing %#v: recorded %#v, want %#v (%v)", step.observed, got, step.want, err)
		}
	}
	// A steady-state reconcile must not need the write lock.
	writer, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	conn, err := writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)
	started := time.Now()
	if err := db.ObserveProjectLeadAgent(ctx, project.ID, LeadAgent{Kind: "pi"}); err != nil {
		t.Fatalf("unchanged observation waited for the write lock: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("unchanged observation took %s while another writer held the lock", elapsed)
	}
}
