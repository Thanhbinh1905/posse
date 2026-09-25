-- +goose Up
CREATE TABLE project_watch_state (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id),
    pr_polled_at INTEGER NOT NULL DEFAULT 0,
    pr_consecutive_failures INTEGER NOT NULL DEFAULT 0,
    checkout_checked_at INTEGER NOT NULL DEFAULT 0,
    root_behind_head TEXT NOT NULL DEFAULT ''
);

CREATE TABLE pr_observations (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    pr_url TEXT NOT NULL,
    head_sha TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT '',
    checks TEXT NOT NULL DEFAULT '',
    review TEXT NOT NULL DEFAULT '',
    mergeable TEXT NOT NULL DEFAULT '',
    merge_commit TEXT NOT NULL DEFAULT '',
    observed_at INTEGER NOT NULL
);

CREATE INDEX pr_observations_task_latest_idx ON pr_observations(task_id, id DESC);

-- +goose Down
DROP TABLE pr_observations;
DROP TABLE project_watch_state;
