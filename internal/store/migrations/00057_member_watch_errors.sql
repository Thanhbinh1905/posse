-- +goose Up
CREATE TABLE member_watch_errors (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo TEXT NOT NULL,
    phase TEXT NOT NULL,
    error TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(project_id, repo, phase)
);

-- +goose Down
DROP TABLE member_watch_errors;
