-- +goose Up
CREATE TABLE publish_pre_push_heads (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    request_url TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, repo, request_url, head_sha)
);

-- +goose Down
DROP TABLE publish_pre_push_heads;
