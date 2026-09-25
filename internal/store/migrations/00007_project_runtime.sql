-- +goose Up
CREATE TABLE project_runtime (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    server_started_at TEXT NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE project_runtime;
