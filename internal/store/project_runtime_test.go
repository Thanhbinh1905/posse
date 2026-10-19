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

func TestLookoutPollerHeartbeatAndFailureNoticeEpisode(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "poller", t.TempDir(), "main")
	if err != nil {
		t.Fatal(err)
	}
	poller := LookoutPoller{PID: 123, Token: "episode-one", StartedAt: 100, HeartbeatAt: 101, ExecutablePath: "/usr/bin/posse", ExecutableDevice: 7, ExecutableInode: 11}
	if err := db.SetLookoutPoller(ctx, project.ID, poller); err != nil {
		t.Fatal(err)
	}
	got, err := db.LookoutPoller(ctx, project.ID)
	if err != nil || got != poller {
		t.Fatalf("Lookout poller = %#v, want %#v: %v", got, poller, err)
	}
	raised, err := db.RaiseLookoutPollerFailure(ctx, project.ID, poller.Token, "Lookout poller heartbeat is stale", 102)
	if err != nil || !raised {
		t.Fatalf("first stale episode notice = %v, %v", raised, err)
	}
	raised, err = db.RaiseLookoutPollerFailure(ctx, project.ID, poller.Token, "duplicate stale poller", 103)
	if err != nil || raised {
		t.Fatalf("duplicate stale episode notice = %v, %v", raised, err)
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil || len(notices) != 1 || notices[0].Kind != "pr_watch_failing" || notices[0].Summary != "Lookout poller heartbeat is stale" {
		t.Fatalf("stale episode Notices = %#v, %v", notices, err)
	}
	if ok, err := db.HeartbeatLookoutPoller(ctx, project.ID, poller.PID, poller.Token, 104); err != nil || !ok {
		t.Fatalf("recovery heartbeat = %v, %v", ok, err)
	}
	if raised, err := db.RaiseLookoutPollerFailure(ctx, project.ID, poller.Token, "second stale episode", 105); err != nil || !raised {
		t.Fatalf("second stale episode notice = %v, %v", raised, err)
	}
	if err := db.ClearLookoutPoller(ctx, project.ID, poller.PID, poller.Token); err != nil {
		t.Fatal(err)
	}
	if got, err := db.LookoutPoller(ctx, project.ID); err != nil || got != (LookoutPoller{}) {
		t.Fatalf("cleared Lookout poller = %#v, %v", got, err)
	}
}

func TestRememberProjectServerStartedAtFillsGenerationAlongsideLookoutRecovery(t *testing.T) {
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
	lookout := LookoutRecovery{PaneID: "w1:p2", RetryAt: 123456, FailedStarts: 2, NoticeRaised: true}
	if err := db.SetLookoutRecovery(ctx, project.ID, lookout); err != nil {
		t.Fatal(err)
	}
	if err := db.RememberProjectServerStartedAt(ctx, project.ID, "server-1"); err != nil {
		t.Fatal(err)
	}
	generation, err := db.ProjectServerStartedAt(ctx, project.ID)
	if err != nil || generation != "server-1" {
		t.Fatalf("remembered Project generation = %q, want server-1: %v", generation, err)
	}
	gotLookout, err := db.LookoutRecovery(ctx, project.ID)
	if err != nil || gotLookout != lookout {
		t.Fatalf("remembering Project generation changed Lookout recovery: %#v, want %#v: %v", gotLookout, lookout, err)
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Observation"})
	if err != nil {
		t.Fatal(err)
	}
	observation := struct {
		paneID, workspaceID, agentSession, agentSessionKind string
		absentSince, idleSince                              int64
		serverStartedAt                                     string
	}{"w1:p1", "w1", "session-1", "claude", 10, 20, "server-1"}
	if err := db.UpdateTaskObservation(ctx, taskID, observation.paneID, observation.workspaceID, observation.agentSession, observation.agentSessionKind, observation.absentSince, observation.idleSince, observation.serverStartedAt); err != nil {
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
	if err := db.UpdateTaskObservation(writeCtx, taskID, observation.paneID, observation.workspaceID, observation.agentSession, observation.agentSessionKind, observation.absentSince, observation.idleSince, observation.serverStartedAt); err != nil {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Observation"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET updated_at=1 WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskObservation(ctx, taskID, "w2:p3", "w2", "session-2", "claude", 30, 40, "server-2"); err != nil {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "ship", Title: "Observation"})
	if err != nil {
		t.Fatal(err)
	}
	observation := struct {
		paneID, workspaceID, agentSession, agentSessionKind string
		absentSince, idleSince                              int64
		serverStartedAt                                     string
	}{"w2:p3", "w2", "session-2", "claude", 30, 40, "server-2"}
	unchanged, err := db.taskObservationMatches(ctx, taskID, observation.paneID, observation.workspaceID, observation.agentSession, observation.agentSessionKind, observation.absentSince, observation.idleSince, observation.serverStartedAt)
	if err != nil || unchanged {
		t.Fatalf("initial Task observation check = unchanged:%t error:%v, want changed", unchanged, err)
	}
	if _, err := writer.ExecContext(ctx, `UPDATE tasks SET pane_id=?,herdr_workspace_id=?,agent_session=?,agent_session_kind=?,agent_absent_since=?,idle_since=?,agent_server_started_at=?,updated_at=100 WHERE id=?`,
		observation.paneID, observation.workspaceID, observation.agentSession, observation.agentSessionKind, observation.absentSince, observation.idleSince, observation.serverStartedAt, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.writeTaskObservation(ctx, taskID, observation.paneID, observation.workspaceID, observation.agentSession, observation.agentSessionKind, observation.absentSince, observation.idleSince, observation.serverStartedAt, 200); err != nil {
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

func TestLookoutRecoveryPersistsAcrossStoreReopen(t *testing.T) {
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
	want := LookoutRecovery{PaneID: "w1:p2", RetryAt: 123456, FailedStarts: 4, NoticeRaised: true}
	if err := db.SetLookoutRecovery(ctx, project.ID, want); err != nil {
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
	got, err := db.LookoutRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened Lookout recovery = %#v, want %#v", got, want)
	}
	if err := db.ResetLookoutRecovery(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	got, err = db.LookoutRecovery(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != (LookoutRecovery{}) {
		t.Fatalf("reset Lookout recovery = %#v, want empty state", got)
	}
}
