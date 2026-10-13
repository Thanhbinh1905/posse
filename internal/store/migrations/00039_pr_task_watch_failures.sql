-- +goose Up
CREATE TABLE pr_task_watch_failures (
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(task_id, repo)
);

-- +goose Down
DROP TABLE pr_task_watch_failures;
