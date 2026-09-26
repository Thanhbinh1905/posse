-- +goose Up
CREATE TABLE decisions (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    origin TEXT NOT NULL,
    question TEXT NOT NULL,
    options_json TEXT NOT NULL,
    answer TEXT NOT NULL DEFAULT '',
    user_quote TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    answered_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE(project_id, origin)
);
CREATE INDEX decisions_project_pending_idx ON decisions(project_id, answered_at, id);

-- +goose Down
DROP TABLE decisions;
