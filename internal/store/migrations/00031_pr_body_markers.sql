-- +goose Up
CREATE TABLE pr_body_markers (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    marker_token TEXT NOT NULL UNIQUE,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(task_id, repo)
);

-- +goose Down
DROP TABLE pr_body_markers;
