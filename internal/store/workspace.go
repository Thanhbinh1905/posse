package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Project kinds. A repo Project is one Git repository; a workspace Project is a
// plain folder whose direct child repositories are its members.
const (
	ProjectKindRepo      = "repo"
	ProjectKindWorkspace = "workspace"
	ProjectKindDirectory = "directory"
)

// Member repository states. A member that disappears from the workspace folder
// is kept as missing because Tasks may still reference it.
const (
	RepoActive  = "active"
	RepoMissing = "missing"
)

// Task member states. A member without commits ahead of its base is unchanged
// and never Merges; the others record a merge head (local) or enter merging (pr) -> merged.
const (
	TaskRepoOpen      = "open"
	TaskRepoUnchanged = "unchanged"
	TaskRepoGated     = "gated"
	TaskRepoMerge     = "merging"
	TaskRepoMerged    = "merged"
)

type ProjectRepo struct {
	ID            int64  `toml:"id"`
	ProjectID     int64  `toml:"project_id"`
	Name          string `toml:"name"`
	Path          string `toml:"path"` // relative to the Project root
	DefaultBranch string `toml:"default_branch"`
	Status        string `toml:"status"`
	OriginHost    string `toml:"origin_host"`
}

type RepoWatchState struct {
	CheckoutCheckedAt int64
	CheckoutStatus    string
	CheckoutReason    string
}

type TaskRepo struct {
	ID           int64  `toml:"id"`
	TaskID       int64  `toml:"task_id"`
	Repo         string `toml:"repo"`
	WorktreePath string `toml:"worktree_path"`
	BaseRef      string `toml:"base_ref"`
	TargetBranch string `toml:"target_branch"`
	MergeMode    string `toml:"landing_mode"`
	State        string `toml:"state"`
	GatedSHA     string `toml:"gated_sha"`
	PRURL        string `toml:"pr_url"`
	MergedRef    string `toml:"landed_ref"`
}

type TaskTargetChange struct {
	ID           int64
	ProjectID    int64
	TaskID       int64
	Repo         string
	SourceTaskID int64
	FromBranch   string
	ToBranch     string
	PRURL        string
	Status       string
	Error        string
	CreatedAt    int64
	CompletedAt  int64
}

var ErrTargetBranchChanged = errors.New("Task target branch changed during stacked retarget")

func (p Project) IsWorkspace() bool { return p.Kind == ProjectKindWorkspace }
func (p Project) IsDirectory() bool { return p.Kind == ProjectKindDirectory }

// CreateDirectoryProject registers an explicit local folder without Git metadata.
func (db *DB) CreateDirectoryProject(ctx context.Context, name, root string) (Project, error) {
	uuid, err := newProjectUUID()
	if err != nil {
		return Project{}, err
	}
	now := time.Now().UnixMilli()
	result, err := db.ExecContext(ctx, `INSERT INTO projects(name, root, default_branch, kind, status, created_at, last_activity_at, project_uuid) VALUES (?, ?, '', ?, 'active', ?, ?, ?)`, name, root, ProjectKindDirectory, now, now, uuid)
	if err != nil {
		return Project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	project := Project{ID: id, UUID: uuid, Name: name, Root: root, Kind: ProjectKindDirectory, Status: "active", CreatedAt: now, LastActivityAt: now}
	if err := db.PersistProject(ctx, id); err != nil {
		return Project{}, err
	}
	return project, nil
}

// CreateWorkspaceProject registers a workspace Project and its members in one transaction.
func (db *DB) CreateWorkspaceProject(ctx context.Context, name, root string, repos []ProjectRepo) (Project, error) {
	uuid, err := newProjectUUID()
	if err != nil {
		return Project{}, err
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO projects(name, root, default_branch, kind, status, created_at, last_activity_at, project_uuid) VALUES (?, ?, '', ?, 'active', ?, ?, ?)`, name, root, ProjectKindWorkspace, now, now, uuid)
	if err != nil {
		return Project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	for _, repo := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos(project_id, name, path, default_branch, status, created_at, updated_at, origin_host) VALUES (?, ?, ?, ?, 'active', ?, ?, ?)`, id, repo.Name, repo.Path, repo.DefaultBranch, now, now, repo.OriginHost); err != nil {
			return Project{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Project{}, err
	}
	project := Project{ID: id, UUID: uuid, Name: name, Root: root, Kind: ProjectKindWorkspace, Status: "active", CreatedAt: now, LastActivityAt: now}
	if err := db.PersistProject(ctx, id); err != nil {
		return Project{}, err
	}
	return project, nil
}

// ProjectRepos lists a Project's members by name, including missing ones.
func (db *DB) ProjectRepos(ctx context.Context, projectID int64) ([]ProjectRepo, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, project_id, name, path, default_branch, status, origin_host FROM project_repos WHERE project_id=? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []ProjectRepo{}
	for rows.Next() {
		var repo ProjectRepo
		if err := rows.Scan(&repo.ID, &repo.ProjectID, &repo.Name, &repo.Path, &repo.DefaultBranch, &repo.Status, &repo.OriginHost); err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

// SyncProjectRepos makes the stored members match a fresh scan: new members are
// added, known ones refreshed and reactivated, and absent ones marked missing.
func (db *DB) SyncProjectRepos(ctx context.Context, projectID int64, scanned []ProjectRepo) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	seen := map[string]bool{}
	for _, repo := range scanned {
		seen[repo.Name] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos(project_id, name, path, default_branch, status, created_at, updated_at, origin_host) VALUES (?, ?, ?, ?, 'active', ?, ?, ?)
ON CONFLICT(project_id, name) DO UPDATE SET path=excluded.path, default_branch=excluded.default_branch, origin_host=excluded.origin_host, status='active', updated_at=excluded.updated_at`, projectID, repo.Name, repo.Path, repo.DefaultBranch, now, now, repo.OriginHost); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM project_repos WHERE project_id=? AND status='active'`, projectID)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	rows.Close()
	for _, name := range gone {
		if _, err := tx.ExecContext(ctx, `UPDATE project_repos SET status='missing', updated_at=? WHERE project_id=? AND name=?`, now, projectID, name); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistProject(ctx, projectID)
}

// CreateTaskRepos records the members a workspace Task touches.
func (db *DB) CreateTaskRepos(ctx context.Context, taskID int64, repos []TaskRepo) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, repo := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_repos(task_id, repo, worktree_path, base_ref, target_branch, landing_mode, state, updated_at) VALUES (?, ?, ?, ?, ?, ?, 'open', ?)
ON CONFLICT(task_id, repo) DO UPDATE SET worktree_path=excluded.worktree_path, base_ref=excluded.base_ref, target_branch=excluded.target_branch, landing_mode=excluded.landing_mode, updated_at=excluded.updated_at`, taskID, repo.Repo, repo.WorktreePath, repo.BaseRef, repo.TargetBranch, repo.MergeMode, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, taskID)
}

func (db *DB) TaskRepos(ctx context.Context, taskID int64) ([]TaskRepo, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, task_id, repo, worktree_path, base_ref, target_branch, landing_mode, state, gated_sha, pr_url, landed_ref FROM task_repos WHERE task_id=? ORDER BY repo`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []TaskRepo{}
	for rows.Next() {
		var repo TaskRepo
		if err := rows.Scan(&repo.ID, &repo.TaskID, &repo.Repo, &repo.WorktreePath, &repo.BaseRef, &repo.TargetBranch, &repo.MergeMode, &repo.State, &repo.GatedSHA, &repo.PRURL, &repo.MergedRef); err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

// UpdateTaskRepo stores one member's merging progress.
func (db *DB) UpdateTaskRepo(ctx context.Context, repo TaskRepo) error {
	result, err := db.ExecContext(ctx, `UPDATE task_repos SET state=?, gated_sha=?, pr_url=?, landed_ref=?, updated_at=? WHERE task_id=? AND repo=?`, repo.State, repo.GatedSHA, repo.PRURL, repo.MergedRef, time.Now().UnixMilli(), repo.TaskID, repo.Repo)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return sql.ErrNoRows
	}
	return db.PersistTask(ctx, repo.TaskID)
}

func (db *DB) ProjectRepoWatchState(ctx context.Context, projectID int64, repo string) (RepoWatchState, error) {
	var state RepoWatchState
	err := db.QueryRowContext(ctx, `SELECT checkout_checked_at, checkout_status, checkout_reason FROM repo_watch_state WHERE project_id=? AND repo=?`, projectID, repo).
		Scan(&state.CheckoutCheckedAt, &state.CheckoutStatus, &state.CheckoutReason)
	if errors.Is(err, sql.ErrNoRows) {
		return RepoWatchState{}, nil
	}
	return state, err
}

func (db *DB) RecordRepoCheckout(ctx context.Context, projectID int64, repo string, at int64, status, reason string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO repo_watch_state(project_id, repo, checkout_checked_at, checkout_status, checkout_reason) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(project_id, repo) DO UPDATE SET checkout_checked_at=excluded.checkout_checked_at, checkout_status=excluded.checkout_status, checkout_reason=excluded.checkout_reason`, projectID, repo, at, status, reason)
	return err
}

func (db *DB) SetRepoRootBehindHead(ctx context.Context, projectID int64, repo, head string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO repo_watch_state(project_id, repo, root_behind_head) VALUES (?, ?, ?) ON CONFLICT(project_id, repo) DO UPDATE SET root_behind_head=excluded.root_behind_head`, projectID, repo, head)
	return err
}

// LatestMemberPRObservation is the last observation of one member's pull request.
func (db *DB) LatestMemberPRObservation(ctx context.Context, taskID int64, repo string) (PRObservation, error) {
	var observation PRObservation
	err := db.QueryRowContext(ctx, `SELECT id, project_id, task_id, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at FROM pr_observations WHERE task_id=? AND repo=? ORDER BY id DESC LIMIT 1`, taskID, repo).
		Scan(&observation.ID, &observation.ProjectID, &observation.TaskID, &observation.PRURL, &observation.HeadSHA, &observation.State, &observation.Checks, &observation.Review, &observation.Mergeable, &observation.MergeCommit, &observation.ObservedAt)
	return observation, err
}

// MemberPREffect is what one observation of a member's pull request changes.
type MemberPREffect struct {
	Notices   []Notice
	RepoState string // TaskRepoMerged, TaskRepoOpen or "" for no change
	MergedRef string
}

// RecordMemberPRObservation stores a new observation of a member's pull request,
// its Notices and the member's new state in one transaction, then settles the
// Task: merged once every changed member has Merged, done when a member's pull
// request closed without merging.
func (db *DB) RecordMemberPRObservation(ctx context.Context, repo string, observation PRObservation, effect MemberPREffect) (bool, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_watch_state(project_id) VALUES (?)`, observation.ProjectID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_watch_state SET pr_polled_at=pr_polled_at WHERE project_id=?`, observation.ProjectID); err != nil {
		return false, err
	}
	var currentPRURL, currentRepoState string
	if err := tx.QueryRowContext(ctx, `SELECT pr_url,state FROM task_repos WHERE task_id=? AND repo=?`, observation.TaskID, repo).Scan(&currentPRURL, &currentRepoState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	var currentTaskState State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=? AND project_id=?`, observation.TaskID, observation.ProjectID).Scan(&currentTaskState); err != nil {
		return false, err
	}
	if currentPRURL != observation.PRURL || currentRepoState == TaskRepoMerged {
		return false, nil
	}
	recoveredWatch, err := resetPRTaskWatchFailuresTx(ctx, tx, observation.ProjectID, observation.TaskID, repo, observation.PRURL, observation.ObservedAt)
	if err != nil {
		return false, err
	}
	var previous PRObservation
	err = tx.QueryRowContext(ctx, `SELECT project_id, task_id, pr_url, head_sha, state, checks, review, mergeable, merge_commit FROM pr_observations WHERE task_id=? AND repo=? ORDER BY id DESC LIMIT 1`, observation.TaskID, repo).
		Scan(&previous.ProjectID, &previous.TaskID, &previous.PRURL, &previous.HeadSHA, &previous.State, &previous.Checks, &previous.Review, &previous.Mergeable, &previous.MergeCommit)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	unchanged := err == nil && samePRObservation(previous, observation)
	noticesToRecord := effect.Notices
	if unchanged {
		noticesToRecord = make([]Notice, 0, len(effect.Notices))
		for _, notice := range effect.Notices {
			if notice.Kind == "merge_ready" {
				noticesToRecord = append(noticesToRecord, notice)
			}
		}
	}
	notices, err := filterMergeReadyNoticesTx(ctx, tx, observation.ProjectID, observation.TaskID, repo, currentTaskState, noticesToRecord)
	if err != nil {
		return false, err
	}
	if unchanged && effect.RepoState == "" && len(notices) == 0 {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		if recoveredWatch {
			return true, db.PersistTask(ctx, observation.TaskID)
		}
		return false, nil
	}
	if !unchanged {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pr_observations(project_id, task_id, repo, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, observation.ProjectID, observation.TaskID, repo, observation.PRURL, observation.HeadSHA, observation.State, observation.Checks, observation.Review, observation.Mergeable, observation.MergeCommit, observation.ObservedAt); err != nil {
			return false, err
		}
	}
	for _, notice := range notices {
		if err := insertNoticeTx(ctx, tx, notice); err != nil {
			return false, err
		}
	}
	if effect.RepoState != "" {
		now := time.Now().UnixMilli()
		switch effect.RepoState {
		case TaskRepoMerged:
			_, err = tx.ExecContext(ctx, `UPDATE task_repos SET state=?, landed_ref=?, updated_at=? WHERE task_id=? AND repo=? AND state IN ('merging','open','gated')`, TaskRepoMerged, effect.MergedRef, now, observation.TaskID, repo)
		case TaskRepoOpen:
			_, err = tx.ExecContext(ctx, `UPDATE task_repos SET state=?, gated_sha='', updated_at=? WHERE task_id=? AND repo=? AND state=?`, TaskRepoOpen, now, observation.TaskID, repo, TaskRepoMerge)
		}
		if err != nil {
			return false, err
		}
		_, err = settleWorkspaceTaskTx(ctx, db, tx, observation.TaskID, effect.RepoState == TaskRepoOpen, "Pull request for "+repo+" closed without merging")
		if err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, db.PersistTask(ctx, observation.TaskID)
}

// SettleWorkspaceTask moves a merging workspace Task to merged once every changed
// member has Merged. It reports whether the Task moved.
func (db *DB) SettleWorkspaceTask(ctx context.Context, taskID int64) (bool, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	moved, err := settleWorkspaceTaskTx(ctx, db, tx, taskID, false, "")
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if moved {
		return true, db.PersistTask(ctx, taskID)
	}
	return false, nil
}

func settleWorkspaceTaskTx(ctx context.Context, db *DB, tx *writeTx, taskID int64, reopen bool, reopenNote string) (bool, error) {
	var state State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, taskID).Scan(&state); err != nil {
		return false, err
	}
	if state == StateMerged || state == StateTornDown || state == StateReported {
		return false, nil
	}
	if reopen {
		return true, transitionTx(ctx, db.queries.WithTx(tx.Tx), taskID, StateMerging, StateDone, "cli", reopenNote)
	}
	rows, err := tx.QueryContext(ctx, `SELECT repo, state, landed_ref FROM task_repos WHERE task_id=? ORDER BY repo`, taskID)
	if err != nil {
		return false, err
	}
	merged := []string{}
	for rows.Next() {
		var repo, repoState, ref string
		if err := rows.Scan(&repo, &repoState, &ref); err != nil {
			rows.Close()
			return false, err
		}
		switch repoState {
		case TaskRepoUnchanged:
		case TaskRepoMerged:
			merged = append(merged, repo+"@"+ref)
		default:
			rows.Close()
			return false, nil
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(merged) == 0 {
		return false, nil
	}
	note := "Every member Merged: " + strings.Join(merged, ", ")
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET landed_ref=?, updated_at=? WHERE id=?`, strings.Join(merged, ","), time.Now().UnixMilli(), taskID); err != nil {
		return false, err
	}
	return true, transitionTx(ctx, db.queries.WithTx(tx.Tx), taskID, state, StateMerged, "cli", note)
}

func (db *DB) BeginTaskTargetChange(ctx context.Context, projectID, taskID int64, repo string, sourceTaskID int64, fromBranch, toBranch string) (TaskTargetChange, error) {
	if taskID == sourceTaskID || fromBranch == "" || toBranch == "" || fromBranch == toBranch {
		return TaskTargetChange{}, nil
	}
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return TaskTargetChange{}, err
	}
	defer tx.Rollback()
	var sourceProjectID int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id=?`, sourceTaskID).Scan(&sourceProjectID); err != nil {
		return TaskTargetChange{}, err
	}
	if sourceProjectID != projectID {
		return TaskTargetChange{}, fmt.Errorf("stacked target source Task belongs to another Project")
	}
	var current, prURL string
	if repo == "" {
		err = tx.QueryRowContext(ctx, `SELECT target_branch, pr_url FROM tasks WHERE id=? AND project_id=?`, taskID, projectID).Scan(&current, &prURL)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT r.target_branch, r.pr_url FROM task_repos r JOIN tasks t ON t.id=r.task_id WHERE r.task_id=? AND t.project_id=? AND r.repo=?`, taskID, projectID, repo).Scan(&current, &prURL)
	}
	if err != nil {
		return TaskTargetChange{}, err
	}
	if current != fromBranch && current != toBranch {
		if err := tx.Commit(); err != nil {
			return TaskTargetChange{}, err
		}
		return TaskTargetChange{}, nil
	}
	if current == fromBranch {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_target_changes(project_id,task_id,repo,source_task_id,from_branch,to_branch,pr_url,status,created_at) VALUES(?,?,?,?,?,?,?,'pending',?) ON CONFLICT(task_id,repo,source_task_id) DO NOTHING`, projectID, taskID, repo, sourceTaskID, fromBranch, toBranch, prURL, time.Now().UnixMilli()); err != nil {
			return TaskTargetChange{}, err
		}
	}
	change, err := scanTaskTargetChange(tx.QueryRowContext(ctx, `SELECT id,project_id,task_id,repo,source_task_id,from_branch,to_branch,pr_url,status,error,created_at,completed_at FROM task_target_changes WHERE task_id=? AND repo=? AND source_task_id=?`, taskID, repo, sourceTaskID))
	if errors.Is(err, sql.ErrNoRows) {
		change, err = TaskTargetChange{}, nil
	}
	if err != nil {
		return TaskTargetChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskTargetChange{}, err
	}
	return change, nil
}

func (db *DB) PendingTaskTargetChanges(ctx context.Context, projectID int64) ([]TaskTargetChange, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,task_id,repo,source_task_id,from_branch,to_branch,pr_url,status,error,created_at,completed_at FROM task_target_changes WHERE project_id=? AND status='pending' ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []TaskTargetChange{}
	for rows.Next() {
		change, err := scanTaskTargetChange(rows)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func (db *DB) TaskTargetChanges(ctx context.Context, taskID int64) ([]TaskTargetChange, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,project_id,task_id,repo,source_task_id,from_branch,to_branch,pr_url,status,error,created_at,completed_at FROM task_target_changes WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []TaskTargetChange{}
	for rows.Next() {
		change, err := scanTaskTargetChange(rows)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func (db *DB) CompleteTaskTargetChange(ctx context.Context, changeID int64) error {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	change, err := scanTaskTargetChange(tx.QueryRowContext(ctx, `SELECT id,project_id,task_id,repo,source_task_id,from_branch,to_branch,pr_url,status,error,created_at,completed_at FROM task_target_changes WHERE id=?`, changeID))
	if err != nil {
		return err
	}
	if change.Status == "complete" {
		return tx.Commit()
	}
	if change.Status != "pending" {
		return fmt.Errorf("target change %d is %s", changeID, change.Status)
	}
	var current string
	if change.Repo == "" {
		err = tx.QueryRowContext(ctx, `SELECT target_branch FROM tasks WHERE id=? AND project_id=?`, change.TaskID, change.ProjectID).Scan(&current)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT r.target_branch FROM task_repos r JOIN tasks t ON t.id=r.task_id WHERE r.task_id=? AND t.project_id=? AND r.repo=?`, change.TaskID, change.ProjectID, change.Repo).Scan(&current)
	}
	if err != nil {
		return err
	}
	if current == change.FromBranch {
		var result sql.Result
		if change.Repo == "" {
			result, err = tx.ExecContext(ctx, `UPDATE tasks SET target_branch=?, updated_at=? WHERE id=? AND target_branch=?`, change.ToBranch, time.Now().UnixMilli(), change.TaskID, change.FromBranch)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE task_repos SET target_branch=?, updated_at=? WHERE task_id=? AND repo=? AND target_branch=?`, change.ToBranch, time.Now().UnixMilli(), change.TaskID, change.Repo, change.FromBranch)
		}
		if err != nil {
			return err
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
			return rowsErr
		} else if rows == 0 {
			if change.Repo == "" {
				err = tx.QueryRowContext(ctx, `SELECT target_branch FROM tasks WHERE id=? AND project_id=?`, change.TaskID, change.ProjectID).Scan(&current)
			} else {
				err = tx.QueryRowContext(ctx, `SELECT target_branch FROM task_repos WHERE task_id=? AND repo=?`, change.TaskID, change.Repo).Scan(&current)
			}
			if err != nil {
				return err
			}
			if current != change.ToBranch {
				return ErrTargetBranchChanged
			}
		}
	} else if current != change.ToBranch {
		return ErrTargetBranchChanged
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_target_changes SET status='complete',error='',completed_at=? WHERE id=? AND status='pending'`, time.Now().UnixMilli(), changeID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.PersistTask(ctx, change.TaskID)
}

func (db *DB) BlockTaskTargetChange(ctx context.Context, changeID int64, cause error) error {
	if cause == nil {
		return errors.New("target change block reason is required")
	}
	_, err := db.ExecContext(ctx, `UPDATE task_target_changes SET status='blocked',error=? WHERE id=? AND status='pending'`, cause.Error(), changeID)
	return err
}

func (db *DB) SkipTaskTargetChange(ctx context.Context, changeID int64, cause error) error {
	if cause == nil {
		return errors.New("target change skip reason is required")
	}
	_, err := db.ExecContext(ctx, `UPDATE task_target_changes SET status='skipped',error=?,completed_at=? WHERE id=? AND status='pending'`, cause.Error(), time.Now().UnixMilli(), changeID)
	return err
}

type taskTargetChangeScanner interface {
	Scan(dest ...any) error
}

func scanTaskTargetChange(scanner taskTargetChangeScanner) (TaskTargetChange, error) {
	var change TaskTargetChange
	err := scanner.Scan(&change.ID, &change.ProjectID, &change.TaskID, &change.Repo, &change.SourceTaskID, &change.FromBranch, &change.ToBranch, &change.PRURL, &change.Status, &change.Error, &change.CreatedAt, &change.CompletedAt)
	return change, err
}
