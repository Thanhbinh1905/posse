package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestTaskSnapshotsRebuildStateAndDailyBackupRetention(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Snapshot", MergeMode: "local", AgentSession: `{"session_id":"pi-session"}`, AgentSessionKind: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transition(ctx, taskID, StateSpawning, StateWorking, "cli", "started"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskContainment(ctx, taskID, "marker", "", "task-marker-snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTaskPaneShellIdentity(ctx, taskID, "dev:inode:123"); err != nil {
		t.Fatal(err)
	}
	snapshotPath := db.TaskSnapshotPath(project.Name, 1)
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil || !strings.Contains(string(snapshot), `state = "working"`) || !strings.Contains(string(snapshot), `pane_shell_identity = "dev:inode:123"`) {
		t.Fatalf("atomic Task snapshot = %s, %v", snapshot, err)
	}
	backupDir := filepath.Join(home, "backup")
	backupPath := filepath.Join(backupDir, "posse-"+time.Now().Format("2006-01-02")+".db")
	if info, err := os.Stat(backupPath); err != nil || info.Size() == 0 {
		t.Fatalf("daily VACUUM backup = %#v, %v", info, err)
	}
	for day := 1; day <= 9; day++ {
		name := fmt.Sprintf("posse-2020-01-%02d.db", day)
		if err := os.WriteFile(filepath.Join(backupDir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.BackupDaily(time.Now()); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(filepath.Join(backupDir, "posse-*.db"))
	if err != nil || len(backups) != 7 {
		t.Fatalf("backup retention kept %d files: %v (%v)", len(backups), backups, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='failed' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	count, err := db.RebuildFromSnapshots(ctx, home)
	if err != nil || count != 1 {
		t.Fatalf("rebuild count = %d, %v", count, err)
	}
	rebuilt, err := db.Task(ctx, project.ID, "t1")
	if err != nil || rebuilt.State != StateWorking || rebuilt.AgentSession != `{"session_id":"pi-session"}` || rebuilt.AgentSessionKind != "pi" || rebuilt.ContainmentMode != "marker" || rebuilt.ContainmentMarker != "task-marker-snapshot" || rebuilt.PaneShellIdentity != "dev:inode:123" {
		t.Fatalf("rebuilt task lost its session owner: %#v, %v", rebuilt, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteEmptyProjectRemovesRecoverySnapshot(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "empty", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteEmptyProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db.ProjectSnapshotPath(project.Name)); !os.IsNotExist(err) {
		t.Fatalf("deleted Project snapshot still exists: %v", err)
	}
	if count, err := db.RebuildFromSnapshots(ctx, home); err != nil || count != 0 {
		t.Fatalf("rebuild resurrected the deleted empty Project: count=%d err=%v", count, err)
	}
	if _, err := db.ProjectByName(ctx, project.Name); !IsNotFound(err) {
		t.Fatalf("deleted Project was restored: %v", err)
	}
}

func TestRebuildPreservesWorkspaceMemberOriginHosts(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateWorkspaceProject(ctx, "workspace", filepath.Join(home, "workspace"), []ProjectRepo{
		{Name: "api", Path: "api", DefaultBranch: "main", Status: RepoActive, OriginHost: "github.com"},
		{Name: "notes", Path: "notes", DefaultBranch: "main", Status: RepoActive, OriginHost: "<no-origin>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Snapshot", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE project_repos SET origin_host='' WHERE project_id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt, err := db.ProjectRepos(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, member := range rebuilt {
		got[member.Name] = member.OriginHost
	}
	want := map[string]string{"api": "github.com", "notes": "<no-origin>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuilt member OriginHost = %#v; want %#v", got, want)
	}
}

func TestTaskLaunchIdentitiesPersistAndRebuildWithoutLosingProfileChanges(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Author", ShortName: "author", Profile: "author", Branch: "posse/author", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, taskID, "author", "model-a", true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, taskID, "reviewer", "model-b", true); err != nil {
		t.Fatal(err)
	}
	if err := db.ChangeTaskProfile(ctx, taskID, "author", "reviewer"); err != nil {
		t.Fatal(err)
	}
	identities, err := db.TaskLaunchIdentities(ctx, taskID)
	want := []TaskLaunchIdentity{
		{TaskID: taskID, LaunchNumber: 1, Profile: "author", ConfiguredModel: "model-a", ModelKnown: true},
		{TaskID: taskID, LaunchNumber: 2, Profile: "reviewer", ConfiguredModel: "model-b", ModelKnown: true},
	}
	if err != nil || !reflect.DeepEqual(identities, want) {
		t.Fatalf("launch identities after Profile change = %#v, %v; want %#v", identities, err, want)
	}
	snapshotData, err := os.ReadFile(db.TaskSnapshotPath(project.Name, 1))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot TaskSnapshot
	if _, err := toml.Decode(string(snapshotData), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.LaunchIdentities, want) {
		t.Fatalf("snapshot launch identities = %#v, want %#v", snapshot.LaunchIdentities, want)
	}
	if snapshot.WorkerTurnToken != "launch:2" {
		t.Fatalf("snapshot Worker turn token = %q, want launch:2", snapshot.WorkerTurnToken)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET profile='damaged',launches=0 WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM task_launch_identities`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM task_turn_tokens`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt, err := db.TaskByID(ctx, project.ID, taskID)
	if err != nil || rebuilt.Profile != "reviewer" || rebuilt.Launches != 2 {
		t.Fatalf("rebuilt Task lost its latest Profile or launch count: %#v, %v", rebuilt, err)
	}
	rebuiltIdentities, err := db.TaskLaunchIdentities(ctx, taskID)
	if err != nil || !reflect.DeepEqual(rebuiltIdentities, want) {
		t.Fatalf("rebuilt launch identities = %#v, %v; want %#v", rebuiltIdentities, err, want)
	}
	rebuiltTurn, err := db.TaskTurnToken(ctx, taskID)
	if err != nil || rebuiltTurn != "launch:2" {
		t.Fatalf("rebuilt Worker turn token = %q, %v; want launch:2", rebuiltTurn, err)
	}
}

func TestRebuildKeepsMissingLaunchIdentityHistoryUnknown(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Legacy", ShortName: "legacy", Profile: "legacy", Branch: "posse/legacy", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextTaskLaunchWithIdentity(ctx, taskID, "legacy", "model-a", true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM task_launch_identities WHERE task_id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistTask(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt, err := db.TaskByID(ctx, project.ID, taskID)
	identities, identityErr := db.TaskLaunchIdentities(ctx, taskID)
	if err != nil || identityErr != nil || rebuilt.Launches != 1 || len(identities) != 0 {
		t.Fatalf("incomplete identity history was inferred during rebuild: task=%#v identities=%#v errors=%v/%v", rebuilt, identities, err, identityErr)
	}
}

func TestRebuildCreatesVACUUMBackupBeforeReplacingState(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Snapshot", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET state='failed' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(filepath.Join(home, "backup", "posse-before-rebuild-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("pre-rebuild backup = %v, %v", backups, err)
	}
	backup, err := OpenAt(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var priorState string
	if err := backup.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, taskID).Scan(&priorState); err != nil || priorState != "failed" {
		t.Fatalf("backup state = %q, %v; want pre-rebuild failed state", priorState, err)
	}
}

func TestBackupDailyRetentionPreservesPreRebuildBackups(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Snapshot", MergeMode: "local"}); err != nil {
		t.Fatal(err)
	}

	for rebuild := 0; rebuild < 9; rebuild++ {
		if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
			t.Fatalf("rebuild %d: %v", rebuild+1, err)
		}
	}
	backupDir := filepath.Join(home, "backup")
	before, err := filepath.Glob(filepath.Join(backupDir, "posse-before-rebuild-*.db"))
	if err != nil || len(before) != 9 {
		t.Fatalf("pre-rebuild backups before daily maintenance = %d: %v (%v)", len(before), before, err)
	}
	if err := db.BackupDaily(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, backup := range before {
		if _, err := os.Stat(backup); err != nil {
			t.Errorf("daily retention removed pre-rebuild backup %s: %v", filepath.Base(backup), err)
		}
	}
}

func TestOpenSurvivesDailyBackupFailure(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "backup"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(home)
	if err != nil {
		t.Fatalf("Open failed because the optional daily backup failed: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM projects").Scan(&count); err != nil {
		t.Fatalf("opened database is not usable: %v", err)
	}
}

func TestBackupDailyReclaimsStaleLock(t *testing.T) {
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backupDir := filepath.Join(home, "backup")
	lockPath := filepath.Join(backupDir, ".backup.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	nextDay := time.Now().Add(24 * time.Hour)
	if err := db.BackupDaily(nextDay); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(backupDir, "posse-"+nextDay.Format("2006-01-02")+".db")
	if info, err := os.Stat(backupPath); err != nil || info.Size() == 0 {
		t.Fatalf("stale lock prevented backup creation: %#v, %v", info, err)
	}
}

func TestRebuildRestoresPRWatchFailureStreakAndClearsIntentRuntimeForeignKeys(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	db, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	project, err := db.CreateProject(ctx, "shop", filepath.Join(home, "repo"), "main")
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "research", Title: "Snapshot", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/shop/pull/1"
	if err := db.UpdateTaskMerge(ctx, taskID, prURL, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordPRTaskWatchFailure(ctx, project.ID, taskID, "", prURL, 1, "offline", "watch failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mounts(project_id,n,path,state,task_id) VALUES(?,1,?,'held',?)`, project.ID, filepath.Join(home, "sandbox"), taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO intents(project_id,task_id,command,step,updated_at) VALUES(?,?,'start','done:task.create',1)`, project.ID, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_runtime(project_id,server_started_at) VALUES(?,'generation-1')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RebuildFromSnapshots(ctx, home); err != nil {
		t.Fatalf("rebuild failed with dependent rows present: %v", err)
	}
	var failureRows, failureCount int
	var failureURL, failureMessage string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(consecutive_failures),0),COALESCE(MAX(pr_url),''),COALESCE(MAX(last_error),'') FROM pr_task_watch_failures`).Scan(&failureRows, &failureCount, &failureURL, &failureMessage); err != nil || failureRows != 1 || failureCount != 1 || failureURL != prURL || failureMessage != "offline" {
		t.Fatalf("rebuild did not restore the PR watch failure streak: rows=%d count=%d url=%q error=%q err=%v", failureRows, failureCount, failureURL, failureMessage, err)
	}
}
