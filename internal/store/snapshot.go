package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/thanhbinh1905/posse/internal/atomicfile"
	"github.com/thanhbinh1905/posse/internal/execgroup"
	dbgen "github.com/thanhbinh1905/posse/internal/store/sqlc"
)

type TaskSnapshot struct {
	Version int     `toml:"version"`
	Project Project `toml:"project"`
	Task    Task    `toml:"task"`
	// LaunchIdentities retain the configured author and reviewer identity for
	// every recorded Task launch. Missing numbers are left missing so rebuilds
	// can report legacy or interrupted history as unknown rather than infer it.
	LaunchIdentities []TaskLaunchIdentity `toml:"launch_identities,omitempty"`
	// Members and TaskRepos are set for a workspace Project only.
	Members   []ProjectRepo `toml:"members,omitempty"`
	TaskRepos []TaskRepo    `toml:"task_repos,omitempty"`
}

func IsCorruptDatabaseFile(path string) bool {
	databaseURL := (&url.URL{Scheme: "file", Path: path}).String()
	database, err := sql.Open("sqlite", databaseURL+"?mode=ro&_pragma=busy_timeout(100)")
	if err != nil {
		return false
	}
	defer database.Close()
	var result string
	if err := database.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		message := strings.ToLower(err.Error())
		return strings.Contains(message, "not a database") || strings.Contains(message, "malformed") || strings.Contains(message, "corrupt")
	}
	return result != "ok"
}

func IsTaskWorktreeSnapshot(home, root string) bool {
	paths, err := filepath.Glob(filepath.Join(home, "projects", "*", "tasks", "t*", "task.toml"))
	if err != nil {
		return false
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var snapshot TaskSnapshot
		if _, err := toml.Decode(string(data), &snapshot); err == nil && snapshot.Task.WorktreePath != "" && filepath.Clean(snapshot.Task.WorktreePath) == filepath.Clean(root) {
			return true
		}
	}
	return false
}

func OpenReadOnly(home string) (*DB, error) {
	if home == "" {
		return nil, fmt.Errorf("POSSE_HOME is empty")
	}
	path := filepath.Join(home, "posse.db")
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	fileURL := (&url.URL{Scheme: "file", Path: path}).String()
	database, err := sql.Open("sqlite", fileURL+"?mode=ro&_pragma=busy_timeout(100)")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.Ping(); err != nil {
		_ = database.Close()
		return nil, err
	}
	return &DB{DB: database, Path: path, queries: dbgen.New(database)}, nil
}

func (db *DB) TaskSnapshotPath(project string, sequence int) string {
	return filepath.Join(filepath.Dir(db.Path), "projects", project, "tasks", fmt.Sprintf("t%d", sequence), "task.toml")
}

func (db *DB) PersistTask(ctx context.Context, taskID int64) error {
	var projectID int64
	if err := db.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=?`, taskID).Scan(&projectID); err != nil {
		return err
	}
	task, err := db.TaskByID(ctx, projectID, taskID)
	if err != nil {
		return err
	}
	project, err := db.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	identities, err := db.TaskLaunchIdentities(ctx, taskID)
	if err != nil {
		return err
	}
	snapshot := TaskSnapshot{Version: 1, Project: project, Task: task, LaunchIdentities: identities}
	if project.IsWorkspace() {
		if snapshot.Members, err = db.ProjectRepos(ctx, projectID); err != nil {
			return err
		}
		if snapshot.TaskRepos, err = db.TaskRepos(ctx, taskID); err != nil {
			return err
		}
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(snapshot); err != nil {
		return err
	}
	return atomicfile.Write(db.TaskSnapshotPath(project.Name, task.Seq), encoded.Bytes(), 0o600)
}

func (db *DB) BackupDaily(now time.Time) error {
	directory := filepath.Join(filepath.Dir(db.Path), "backup")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	backup := filepath.Join(directory, "posse-"+now.Format("2006-01-02")+".db")
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		lockPath := filepath.Join(directory, ".backup.lock")
		lock, lockErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(lockErr, os.ErrExist) {
			if info, statErr := os.Stat(lockPath); statErr == nil && now.Sub(info.ModTime()) > time.Hour {
				if removeErr := os.Remove(lockPath); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
					lock, lockErr = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				}
			}
			if errors.Is(lockErr, os.ErrExist) {
				return nil
			}
		}
		if lockErr != nil {
			return lockErr
		}
		_ = lock.Close()
		defer os.Remove(lockPath)
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			quoted := strings.ReplaceAll(backup, "'", "''")
			if _, err := db.ExecContext(context.Background(), "VACUUM INTO '"+quoted+"'"); err != nil {
				_ = os.Remove(backup)
				return fmt.Errorf("create daily database backup: %w", err)
			}
			if err := os.Chmod(backup, 0o600); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	entries, err := filepath.Glob(filepath.Join(directory, "posse-*.db"))
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(entries)))
	if len(entries) > 7 {
		for _, stale := range entries[7:] {
			if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (db *DB) BackupBeforeRebuild(ctx context.Context, now time.Time) (string, error) {
	directory := filepath.Join(filepath.Dir(db.Path), "backup")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	backup := filepath.Join(directory, "posse-before-rebuild-"+now.UTC().Format("20060102T150405.000000000")+".db")
	quoted := strings.ReplaceAll(backup, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		_ = os.Remove(backup)
		return "", fmt.Errorf("create pre-rebuild database backup: %w", err)
	}
	if err := os.Chmod(backup, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

func (db *DB) RebuildFromSnapshots(ctx context.Context, home string) (int, error) {
	if _, err := db.BackupBeforeRebuild(ctx, time.Now()); err != nil {
		return 0, err
	}
	paths, err := filepath.Glob(filepath.Join(home, "projects", "*", "tasks", "t*", "task.toml"))
	if err != nil {
		return 0, err
	}
	snapshots := make([]TaskSnapshot, 0, len(paths))
	projects := map[int64]Project{}
	members := map[int64][]ProjectRepo{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		var snapshot TaskSnapshot
		if _, err := toml.Decode(string(data), &snapshot); err != nil {
			return 0, fmt.Errorf("decode %s: %w", path, err)
		}
		if snapshot.Version != 1 || snapshot.Project.ID == 0 || snapshot.Project.Name == "" || snapshot.Task.ID == 0 || snapshot.Task.Seq < 1 || snapshot.Task.ProjectID != snapshot.Project.ID {
			return 0, fmt.Errorf("invalid Task snapshot %s", path)
		}
		if filepath.Clean(path) != filepath.Clean(db.TaskSnapshotPath(snapshot.Project.Name, snapshot.Task.Seq)) {
			return 0, fmt.Errorf("Task snapshot path does not match its identity: %s", path)
		}
		projects[snapshot.Project.ID] = snapshot.Project
		if len(snapshot.Members) > 0 && snapshot.Task.UpdatedAt >= latestUpdate(snapshots, snapshot.Project.ID) {
			members[snapshot.Project.ID] = snapshot.Members
		}
		snapshots = append(snapshots, snapshot)
	}
	maxID := int64(0)
	known := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		known[fmt.Sprintf("%d:%d", snapshot.Project.ID, snapshot.Task.Seq)] = true
		if snapshot.Task.ID > maxID {
			maxID = snapshot.Task.ID
		}
	}
	for _, project := range projects {
		recovered, err := recoverBranchSnapshots(ctx, project, known, maxID)
		if err != nil {
			return 0, err
		}
		for _, snapshot := range recovered {
			maxID = snapshot.Task.ID
			known[fmt.Sprintf("%d:%d", snapshot.Project.ID, snapshot.Task.Seq)] = true
			snapshots = append(snapshots, snapshot)
		}
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Task.ID < snapshots[j].Task.ID })
	if err := db.rebuild(ctx, snapshots, projects, members); err != nil {
		return 0, err
	}
	for _, snapshot := range snapshots {
		if err := db.PersistTask(ctx, snapshot.Task.ID); err != nil {
			return 0, err
		}
	}
	return len(snapshots), nil
}

func recoverBranchSnapshots(ctx context.Context, project Project, known map[string]bool, maxID int64) ([]TaskSnapshot, error) {
	branches, err := execgroup.CommandContext(ctx, "git", "-C", project.Root, "branch", "--list", "--format=%(refname:short)", "posse/t*").CombinedOutput()
	if err != nil {
		return nil, nil
	}
	worktrees := map[string]string{}
	output, err := execgroup.CommandContext(ctx, "git", "-C", project.Root, "worktree", "list", "--porcelain").CombinedOutput()
	if err == nil {
		path := ""
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimPrefix(line, "worktree ")
			} else if strings.HasPrefix(line, "branch refs/heads/") && path != "" {
				worktrees[strings.TrimPrefix(line, "branch refs/heads/")] = path
			}
		}
	}
	var recovered []TaskSnapshot
	for _, branch := range strings.Fields(string(branches)) {
		if !strings.HasPrefix(branch, "posse/t") {
			continue
		}
		sequence, parseErr := strconv.Atoi(strings.TrimPrefix(branch, "posse/t"))
		if parseErr != nil || sequence < 1 || known[fmt.Sprintf("%d:%d", project.ID, sequence)] {
			continue
		}
		path := worktrees[branch]
		maxID++
		task := Task{ID: maxID, ProjectID: project.ID, Seq: sequence, Type: "recovered", Title: "Recovered branch " + branch, State: StateLost, LandingMode: "local", AutonomyReview: "ask", AutonomyLand: "ask", Branch: branch, BaseRef: project.DefaultBranch, WorktreePath: path, CreatedAt: time.Now().UnixMilli(), UpdatedAt: time.Now().UnixMilli()}
		recovered = append(recovered, TaskSnapshot{Version: 1, Project: project, Task: task})
	}
	return recovered, nil
}

// latestUpdate is the newest Task update among the snapshots already read for a
// Project, so the newest snapshot's member list wins.
func latestUpdate(snapshots []TaskSnapshot, projectID int64) int64 {
	latest := int64(0)
	for _, snapshot := range snapshots {
		if snapshot.Project.ID == projectID && len(snapshot.Members) > 0 && snapshot.Task.UpdatedAt > latest {
			latest = snapshot.Task.UpdatedAt
		}
	}
	return latest
}

func (db *DB) rebuild(ctx context.Context, snapshots []TaskSnapshot, projects map[int64]Project, members map[int64][]ProjectRepo) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"notice_notifications", "approvals", "events", "messages", "notices", "signals", "transitions", "intents", "mounts", "project_runtime", "lead_start_claims", "task_launch_identities", "task_repos", "project_repos", "repo_watch_state", "pr_observations", "project_watch_state"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET reviews_task_id=NULL"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM tasks"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM projects"); err != nil {
		return err
	}
	for _, project := range projects {
		kind := project.Kind
		if kind == "" {
			kind = ProjectKindRepo
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id,name,root,default_branch,kind,herdr_workspace_id,lead_pane_id,lead_label,lead_absent_since,status,created_at,last_activity_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			project.ID, project.Name, project.Root, project.DefaultBranch, kind, project.HerdrWorkspaceID, project.LeadPaneID, project.LeadLabel, project.LeadAbsentSince, project.Status, project.CreatedAt, project.LastActivityAt); err != nil {
			return err
		}
		for _, member := range members[project.ID] {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos(project_id,name,path,default_branch,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, project.ID, member.Name, member.Path, member.DefaultBranch, member.Status, project.CreatedAt, project.LastActivityAt); err != nil {
				return err
			}
		}
	}
	for _, snapshot := range snapshots {
		task := snapshot.Task
		var reviewed any
		if task.ReviewsTaskID != 0 {
			reviewed = task.ReviewsTaskID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,project_id,seq,type,reviews_task_id,title,short_name,state,profile,dispatch_rule,landing_mode,autonomy_review,autonomy_land,branch,base_ref,worktree_path,herdr_workspace_id,pane_id,pane_label,agent_name,agent_session,pr_url,landed_ref,last_output_hash,last_worktree_hash,last_progress_at,agent_absent_since,idle_since,launches,gated_sha,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			task.ID, task.ProjectID, task.Seq, task.Type, reviewed, task.Title, task.ShortName, task.State, task.Profile, task.DispatchRule, task.LandingMode, task.AutonomyReview, task.AutonomyLand, task.Branch, task.BaseRef, task.WorktreePath, task.HerdrWorkspaceID, task.PaneID, task.PaneLabel, task.AgentName, task.AgentSession, task.PRURL, task.LandedRef, task.LastOutputHash, task.LastWorktreeHash, task.LastProgressAt, task.AgentAbsentSince, task.IdleSince, task.Launches, task.GatedSHA, task.CreatedAt, task.UpdatedAt); err != nil {
			return err
		}
		for _, identity := range snapshot.LaunchIdentities {
			if identity.TaskID != 0 && identity.TaskID != task.ID {
				return fmt.Errorf("Task launch identity in snapshot for t%d belongs to Task %d", task.Seq, identity.TaskID)
			}
			if identity.LaunchNumber < 1 || identity.LaunchNumber > task.Launches {
				return fmt.Errorf("invalid launch number %d in snapshot for t%d", identity.LaunchNumber, task.Seq)
			}
			modelKnown := identity.ModelKnown && strings.TrimSpace(identity.ConfiguredModel) != ""
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_launch_identities(task_id,launch_number,profile_name,configured_model,model_known) VALUES(?,?,?,?,?)`, task.ID, identity.LaunchNumber, identity.Profile, identity.ConfiguredModel, modelKnown); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO transitions(task_id,from_state,to_state,source,note,at) VALUES(?,?,?,?,?,?)`, task.ID, "", task.State, "cli", "Rebuilt from atomic Task snapshot", task.UpdatedAt); err != nil {
			return err
		}
		for _, repo := range snapshot.TaskRepos {
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_repos(task_id,repo,worktree_path,base_ref,landing_mode,state,gated_sha,pr_url,landed_ref,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, task.ID, repo.Repo, repo.WorktreePath, repo.BaseRef, repo.LandingMode, repo.State, repo.GatedSHA, repo.PRURL, repo.LandedRef, task.UpdatedAt); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
