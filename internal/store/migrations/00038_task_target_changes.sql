-- +goose Up
CREATE TABLE task_target_changes (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL DEFAULT '',
    source_task_id INTEGER NOT NULL REFERENCES tasks(id),
    from_branch TEXT NOT NULL,
    to_branch TEXT NOT NULL,
    pr_url TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('pending', 'complete', 'blocked', 'skipped')),
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE(task_id, repo, source_task_id)
);
CREATE INDEX task_target_changes_pending ON task_target_changes(project_id, status, id);

-- +goose Down
DROP TABLE task_target_changes;
