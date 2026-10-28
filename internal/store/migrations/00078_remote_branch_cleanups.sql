-- +goose Up
-- A merged Task branch whose remote deletion failed, kept with the cause so a
-- later watch pass or `posse pool prune` retries it.
CREATE TABLE remote_branch_cleanups (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    branch TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    cause TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 1,
    failed_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, repo)
);

-- +goose Down
DROP TABLE remote_branch_cleanups;
