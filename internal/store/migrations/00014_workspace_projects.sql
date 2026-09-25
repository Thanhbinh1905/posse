-- +goose Up
-- A Project is either one repository (kind 'repo') or a workspace folder whose
-- direct child repositories are its members (kind 'workspace').
ALTER TABLE projects ADD COLUMN kind TEXT NOT NULL DEFAULT 'repo';

CREATE TABLE project_repos (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    name TEXT NOT NULL,
    path TEXT NOT NULL,
    default_branch TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(project_id, name),
    UNIQUE(project_id, path)
);

-- One row per member repository a workspace Task touches.
CREATE TABLE task_repos (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    base_ref TEXT NOT NULL,
    landing_mode TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'open',
    gated_sha TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    landed_ref TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    UNIQUE(task_id, repo)
);

-- Per-member checkout sync state; the project_watch_state row keeps the PR poll.
CREATE TABLE repo_watch_state (
    project_id INTEGER NOT NULL REFERENCES projects(id),
    repo TEXT NOT NULL,
    root_behind_head TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(project_id, repo)
);

ALTER TABLE pr_observations ADD COLUMN repo TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE pr_observations DROP COLUMN repo;
DROP TABLE repo_watch_state;
DROP TABLE task_repos;
DROP TABLE project_repos;
ALTER TABLE projects DROP COLUMN kind;
