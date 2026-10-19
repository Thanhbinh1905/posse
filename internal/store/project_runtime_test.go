package store

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

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
