-- +goose Up
CREATE TABLE repo_fetch_failures (
    project_id INTEGER NOT NULL REFERENCES projects(id),
    repo TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(project_id, repo)
);

-- +goose Down
DROP TABLE repo_fetch_failures;
