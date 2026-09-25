package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Project kinds. A repo Project is one Git repository; a workspace Project is a
// plain folder whose direct child repositories are its members.
const (
	ProjectKindRepo      = "repo"
	ProjectKindWorkspace = "workspace"
)

// Member repository states. A member that disappears from the workspace folder
// is kept as missing because Tasks may still reference it.
const (
	RepoActive  = "active"
	RepoMissing = "missing"
)

// Task member states. A member without commits ahead of its base is unchanged
// and never Lands; the others move open -> gated (local) or landing (pr) -> landed.
const (
	TaskRepoOpen      = "open"
	TaskRepoUnchanged = "unchanged"
	TaskRepoGated     = "gated"
	TaskRepoLanding   = "landing"
	TaskRepoLanded    = "landed"
)

type ProjectRepo struct {
	ID            int64  `toml:"id"`
	ProjectID     int64  `toml:"project_id"`
	Name          string `toml:"name"`
	Path          string `toml:"path"` // relative to the Project root
	DefaultBranch string `toml:"default_branch"`
	Status        string `toml:"status"`
}

type TaskRepo struct {
	ID           int64  `toml:"id"`
	TaskID       int64  `toml:"task_id"`
	Repo         string `toml:"repo"`
	WorktreePath string `toml:"worktree_path"`
	BaseRef      string `toml:"base_ref"`
	LandingMode  string `toml:"landing_mode"`
	State        string `toml:"state"`
	GatedSHA     string `toml:"gated_sha"`
	PRURL        string `toml:"pr_url"`
	LandedRef    string `toml:"landed_ref"`
}

func (p Project) IsWorkspace() bool { return p.Kind == ProjectKindWorkspace }

// CreateWorkspaceProject registers a workspace Project and its members in one transaction.
func (db *DB) CreateWorkspaceProject(ctx context.Context, name, root string, repos []ProjectRepo) (Project, error) {
	tx, err := db.beginTxWithRetry(ctx)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO projects(name, root, default_branch, kind, status, created_at, last_activity_at) VALUES (?, ?, '', ?, 'active', ?, ?)`, name, root, ProjectKindWorkspace, now, now)
	if err != nil {
		return Project{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	for _, repo := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos(project_id, name, path, default_branch, status, created_at, updated_at) VALUES (?, ?, ?, ?, 'active', ?, ?)`, id, repo.Name, repo.Path, repo.DefaultBranch, now, now); err != nil {
			return Project{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Project{}, err
	}
	return Project{ID: id, Name: name, Root: root, Kind: ProjectKindWorkspace, Status: "active", CreatedAt: now, LastActivityAt: now}, nil
}

// ProjectRepos lists a Project's members by name, including missing ones.
func (db *DB) ProjectRepos(ctx context.Context, projectID int64) ([]ProjectRepo, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, project_id, name, path, default_branch, status FROM project_repos WHERE project_id=? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []ProjectRepo{}
	for rows.Next() {
		var repo ProjectRepo
		if err := rows.Scan(&repo.ID, &repo.ProjectID, &repo.Name, &repo.Path, &repo.DefaultBranch, &repo.Status); err != nil {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_repos(project_id, name, path, default_branch, status, created_at, updated_at) VALUES (?, ?, ?, ?, 'active', ?, ?)
ON CONFLICT(project_id, name) DO UPDATE SET path=excluded.path, default_branch=excluded.default_branch, status='active', updated_at=excluded.updated_at`, projectID, repo.Name, repo.Path, repo.DefaultBranch, now, now); err != nil {
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
	return tx.Commit()
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_repos(task_id, repo, worktree_path, base_ref, landing_mode, state, updated_at) VALUES (?, ?, ?, ?, ?, 'open', ?)
ON CONFLICT(task_id, repo) DO UPDATE SET worktree_path=excluded.worktree_path, base_ref=excluded.base_ref, landing_mode=excluded.landing_mode, updated_at=excluded.updated_at`, taskID, repo.Repo, repo.WorktreePath, repo.BaseRef, repo.LandingMode, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) TaskRepos(ctx context.Context, taskID int64) ([]TaskRepo, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, task_id, repo, worktree_path, base_ref, landing_mode, state, gated_sha, pr_url, landed_ref FROM task_repos WHERE task_id=? ORDER BY repo`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []TaskRepo{}
	for rows.Next() {
		var repo TaskRepo
		if err := rows.Scan(&repo.ID, &repo.TaskID, &repo.Repo, &repo.WorktreePath, &repo.BaseRef, &repo.LandingMode, &repo.State, &repo.GatedSHA, &repo.PRURL, &repo.LandedRef); err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

// UpdateTaskRepo stores one member's landing progress.
func (db *DB) UpdateTaskRepo(ctx context.Context, repo TaskRepo) error {
	result, err := db.ExecContext(ctx, `UPDATE task_repos SET state=?, gated_sha=?, pr_url=?, landed_ref=?, updated_at=? WHERE task_id=? AND repo=?`, repo.State, repo.GatedSHA, repo.PRURL, repo.LandedRef, time.Now().UnixMilli(), repo.TaskID, repo.Repo)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return sql.ErrNoRows
	}
	return nil
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
	RepoState string // TaskRepoLanded, TaskRepoOpen or "" for no change
	LandedRef string
}

// RecordMemberPRObservation stores a new observation of a member's pull request,
// its Notices and the member's new state in one transaction, then settles the
// Task: landed once every changed member has Landed, done when a member's pull
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
	var previous PRObservation
	err = tx.QueryRowContext(ctx, `SELECT pr_url, head_sha, state, checks, review, mergeable, merge_commit FROM pr_observations WHERE task_id=? AND repo=? ORDER BY id DESC LIMIT 1`, observation.TaskID, repo).
		Scan(&previous.PRURL, &previous.HeadSHA, &previous.State, &previous.Checks, &previous.Review, &previous.Mergeable, &previous.MergeCommit)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && samePRObservation(previous, observation) {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pr_observations(project_id, task_id, repo, pr_url, head_sha, state, checks, review, mergeable, merge_commit, observed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, observation.ProjectID, observation.TaskID, repo, observation.PRURL, observation.HeadSHA, observation.State, observation.Checks, observation.Review, observation.Mergeable, observation.MergeCommit, observation.ObservedAt); err != nil {
		return false, err
	}
	for _, notice := range effect.Notices {
		if err := insertNoticeTx(ctx, tx, notice); err != nil {
			return false, err
		}
	}
	transitioned := false
	if effect.RepoState != "" {
		now := time.Now().UnixMilli()
		switch effect.RepoState {
		case TaskRepoLanded:
			_, err = tx.ExecContext(ctx, `UPDATE task_repos SET state=?, landed_ref=?, updated_at=? WHERE task_id=? AND repo=? AND state=?`, TaskRepoLanded, effect.LandedRef, now, observation.TaskID, repo, TaskRepoLanding)
		case TaskRepoOpen:
			_, err = tx.ExecContext(ctx, `UPDATE task_repos SET state=?, gated_sha='', updated_at=? WHERE task_id=? AND repo=? AND state=?`, TaskRepoOpen, now, observation.TaskID, repo, TaskRepoLanding)
		}
		if err != nil {
			return false, err
		}
		transitioned, err = settleWorkspaceTaskTx(ctx, db, tx, observation.TaskID, effect.RepoState == TaskRepoOpen, "Pull request for "+repo+" closed without merging")
		if err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if transitioned {
		if err := db.PersistTask(ctx, observation.TaskID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// SettleWorkspaceTask moves a landing workspace Task to landed once every changed
// member has Landed. It reports whether the Task moved.
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

func settleWorkspaceTaskTx(ctx context.Context, db *DB, tx *sql.Tx, taskID int64, reopen bool, reopenNote string) (bool, error) {
	var state State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=?`, taskID).Scan(&state); err != nil {
		return false, err
	}
	if state != StateLanding {
		return false, nil
	}
	if reopen {
		return true, transitionTx(ctx, db.queries.WithTx(tx), taskID, StateLanding, StateDone, "cli", reopenNote)
	}
	rows, err := tx.QueryContext(ctx, `SELECT repo, state, landed_ref FROM task_repos WHERE task_id=? ORDER BY repo`, taskID)
	if err != nil {
		return false, err
	}
	landed := []string{}
	for rows.Next() {
		var repo, repoState, ref string
		if err := rows.Scan(&repo, &repoState, &ref); err != nil {
			rows.Close()
			return false, err
		}
		switch repoState {
		case TaskRepoUnchanged:
		case TaskRepoLanded:
			landed = append(landed, repo+"@"+ref)
		default:
			rows.Close()
			return false, nil
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(landed) == 0 {
		return false, nil
	}
	note := "Every member Landed: " + strings.Join(landed, ", ")
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET landed_ref=?, updated_at=? WHERE id=?`, strings.Join(landed, ","), time.Now().UnixMilli(), taskID); err != nil {
		return false, err
	}
	return true, transitionTx(ctx, db.queries.WithTx(tx), taskID, StateLanding, StateLanded, "cli", note)
}
