-- +goose Up
CREATE TABLE task_budget_usage (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    launch INTEGER NOT NULL,
    session_id TEXT NOT NULL,
    session_kind TEXT NOT NULL,
    tokens INTEGER NOT NULL DEFAULT 0,
    local_day TEXT NOT NULL,
    day_tokens INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(task_id, session_id)
);
CREATE INDEX task_budget_usage_day_idx ON task_budget_usage(local_day, task_id);

CREATE TABLE budget_crossings (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    scope TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(project_id, scope, scope_key)
);

-- +goose Down
DROP TABLE budget_crossings;
DROP TABLE task_budget_usage;
