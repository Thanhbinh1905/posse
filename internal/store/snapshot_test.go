package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestProjectSnapshotBoundsHistoricalNoticeAndDecisionHistory(t *testing.T) {
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
	taskID, err := db.CreateTask(ctx, project.ID, Task{Seq: 1, Type: "change", Title: "Snapshot history", MergeMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO decisions(project_id,task_id,origin,question,options_json,created_at,answered_at,kind,task_launches) VALUES(?,?,?,?,?,?,0,'worker_question',0)`, project.ID, taskID, "active-decision", "Continue?", `["continue","stop"]`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO notices(project_id,kind,summary,data_json,created_at) VALUES(?,'worker_question','active notice','{}',?)`, project.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	insertHistory := func(start, count int) {
		t.Helper()
		for index := start; index < start+count; index++ {
			at := int64(index + 1)
			if _, err := db.ExecContext(ctx, `INSERT INTO decisions(project_id,task_id,origin,question,options_json,answer,user_quote,created_at,answered_at,kind,task_launches) VALUES(?,?,?,?,?,?,?, ?,?,?,?)`, project.ID, taskID, fmt.Sprintf("history-decision-%d", index), "Continue?", `["continue","stop"]`, "continue", "User answered", at, at, "worker_question", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO notices(project_id,kind,summary,data_json,created_at,delivered_at,acked_at) VALUES(?,'worker_question',?,?,?, ?, ?)`, project.ID, fmt.Sprintf("history notice %d", index), `{}`, at, at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	insertHistory(0, projectSnapshotHistoryLimit*2)
	if err := db.PersistProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(db.ProjectSnapshotPath(project.Name))
	if err != nil {
		t.Fatal(err)
	}
	var firstSnapshot projectSnapshot
	if _, err := toml.Decode(string(first), &firstSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(firstSnapshot.State.Decisions) != projectSnapshotHistoryLimit+1 || len(firstSnapshot.State.Notices) != projectSnapshotHistoryLimit+1 {
		t.Fatalf("bounded Project history = decisions %d, Notices %d; want %d each plus the active record", len(firstSnapshot.State.Decisions), len(firstSnapshot.State.Notices), projectSnapshotHistoryLimit)
	}
	if firstSnapshot.State.Decisions[0].Origin != "active-decision" || firstSnapshot.State.Notices[0].Summary != "active notice" {
		t.Fatalf("actionable history was pruned: decisions=%#v notices=%#v", firstSnapshot.State.Decisions[0], firstSnapshot.State.Notices[0])
	}
	firstSize := len(first)
	legacyFirstSize, err := unboundedProjectSnapshotSize(ctx, db, project)
	if err != nil {
		t.Fatal(err)
	}
	insertHistory(projectSnapshotHistoryLimit*2, projectSnapshotHistoryLimit*2)
	if err := db.PersistProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(db.ProjectSnapshotPath(project.Name))
	if err != nil {
		t.Fatal(err)
	}
	var secondSnapshot projectSnapshot
	if _, err := toml.Decode(string(second), &secondSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(secondSnapshot.State.Decisions) != projectSnapshotHistoryLimit+1 || len(secondSnapshot.State.Notices) != projectSnapshotHistoryLimit+1 {
		t.Fatalf("Project history grew after another batch: decisions %d, Notices %d", len(secondSnapshot.State.Decisions), len(secondSnapshot.State.Notices))
	}
	legacySecondSize, err := unboundedProjectSnapshotSize(ctx, db, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Project snapshot history: bounded first=%d bytes, bounded second=%d bytes, unbounded first=%d bytes, unbounded second=%d bytes", firstSize, len(second), legacyFirstSize, legacySecondSize)
	if len(second)-firstSize > 256 || firstSize-len(second) > 256 {
		t.Fatalf("snapshot size grew with discarded history: first=%d second=%d", firstSize, len(second))
	}
	if legacySecondSize <= legacyFirstSize {
		t.Fatalf("unbounded comparison did not grow: first=%d second=%d", legacyFirstSize, legacySecondSize)
	}
}

func unboundedProjectSnapshotSize(ctx context.Context, db *DB, project Project) (int, error) {
	state, err := db.projectStateSnapshot(ctx, project.ID)
	if err != nil {
		return 0, err
	}
	state.Decisions = nil
	rows, err := db.QueryContext(ctx, decisionSelect+` WHERE project_id=? ORDER BY id`, project.ID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		decision, err := decisionRow(rows)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		state.Decisions = append(state.Decisions, decisionSnapshot(decision))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	notices, err := db.Notices(ctx, project.ID, false)
	if err != nil {
		return 0, err
	}
	state.Notices = make([]noticeSnapshot, 0, len(notices))
	for _, notice := range notices {
		state.Notices = append(state.Notices, noticeSnapshot(notice))
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(projectSnapshot{Version: 1, Project: project, State: state}); err != nil {
		return 0, err
	}
	return encoded.Len(), nil
}

func TestProjectSnapshotWriteRechecksIdentityAfterLock(t *testing.T) {
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
	var staleSnapshot projectSnapshot
	if _, err := toml.DecodeFile(db.ProjectSnapshotPath(project.Name), &staleSnapshot); err != nil {
		t.Fatal(err)
	}

	lockPath := db.Path + ".project-" + strconv.FormatInt(project.ID, 10) + ".snapshot.lock"
	lock, err := acquireStoreFileLock(ctx, lockPath, "test Project snapshot")
	if err != nil {
		t.Fatal(err)
	}
	movedRoot := filepath.Join(home, "moved-repo")
	if _, err := db.ExecContext(ctx, `UPDATE projects SET root=? WHERE id=?`, movedRoot, project.ID); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	blockedCtx, cancel := context.WithTimeout(ctx, 75*time.Millisecond)
	defer cancel()
	if err := db.writeProjectSnapshot(blockedCtx, project.ID, staleSnapshot); !errors.Is(err, context.DeadlineExceeded) {
		_ = lock.Close()
		t.Fatalf("snapshot write while lock held = %v, want deadline", err)
	}
	started := time.Now()
	if err := db.writeProjectSnapshot(ctx, project.ID, staleSnapshot); !errors.Is(err, ErrBusy) {
		_ = lock.Close()
		t.Fatalf("unbounded snapshot write while lock held = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		_ = lock.Close()
		t.Fatalf("snapshot lock wait took %s, want bounded wait", elapsed)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.writeProjectSnapshot(ctx, project.ID, staleSnapshot); err != nil {
		t.Fatal(err)
	}
	var snapshot projectSnapshot
	if _, err := toml.DecodeFile(db.ProjectSnapshotPath(project.Name), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Project.Root != movedRoot {
		t.Fatalf("Project snapshot root=%q, want %q", snapshot.Project.Root, movedRoot)
	}
}

// Every command reconciles each Task and persists the Project snapshot. Only a
// state change may rewrite it; otherwise each pass would fsync identical data.
func TestUnchangedProjectSnapshotIsNotRewritten(t *testing.T) {
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
	path := db.ProjectSnapshotPath(project.Name)
	capturedAt := func() int64 {
		t.Helper()
		var snapshot projectSnapshot
		if _, err := toml.DecodeFile(path, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot.State.CapturedAt
	}
	if err := db.PersistProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	firstCapture := capturedAt()
	if err := db.PersistProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || capturedAt() != firstCapture {
		t.Fatal("unchanged Project state rewrote its snapshot")
	}
	if _, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "worker_idle", Summary: "changed"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	if capturedAt() <= firstCapture {
		t.Fatal("changed Project state kept the previous snapshot capture time")
	}
}

// The unchanged check compares a digest recorded in the snapshot header
// instead of decoding the whole snapshot. It must still notice an in-place
// edit that keeps every row count, and a legacy snapshot without a digest.
func TestProjectSnapshotDigestDetectsInPlaceEditsAndLegacySnapshots(t *testing.T) {
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
	noticeID, err := db.CreateNotice(ctx, Notice{ProjectID: project.ID, Kind: "worker_idle", Summary: "before"})
	if err != nil {
		t.Fatal(err)
	}
	path := db.ProjectSnapshotPath(project.Name)
	read := func() projectSnapshot {
		t.Helper()
		var snapshot projectSnapshot
		if _, err := toml.DecodeFile(path, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	first := read()
	if first.StateDigest == "" || recordedProjectSnapshotDigest(path) != first.StateDigest {
		t.Fatalf("snapshot header digest = %q, decoded digest = %q", recordedProjectSnapshotDigest(path), first.StateDigest)
	}

	if _, err := db.ExecContext(ctx, `UPDATE notices SET summary='after' WHERE id=?`, noticeID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE projects SET project_uuid=? WHERE id=?`, "00000000-0000-4000-8000-000000000428", project.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	edited := read()
	if edited.StateDigest == first.StateDigest || edited.State.CapturedAt <= first.State.CapturedAt || edited.Project.UUID != "00000000-0000-4000-8000-000000000428" {
		t.Fatalf("in-place edit was not persisted: first=%#v edited=%#v", first, edited)
	}
	var summary string
	for _, notice := range edited.State.Notices {
		if notice.ID == noticeID {
			summary = notice.Summary
		}
	}
	if summary != "after" {
		t.Fatalf("edited Notice summary in snapshot = %q, want after", summary)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(data), "state_digest = \""+edited.StateDigest+"\"\n", "", 1)
	if legacy == string(data) {
		t.Fatal("snapshot did not record its digest in the header")
	}
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.PersistProject(ctx, project.ID); err != nil {
		t.Fatal(err)
	}
	if recordedProjectSnapshotDigest(path) != edited.StateDigest {
		t.Fatalf("legacy snapshot without a digest was not rewritten: header digest %q", recordedProjectSnapshotDigest(path))
	}
}
