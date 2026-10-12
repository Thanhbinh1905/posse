-- +goose Up
CREATE TABLE model_error_episodes (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    episode INTEGER NOT NULL,
    launch INTEGER NOT NULL,
    agent TEXT NOT NULL,
    kind TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    turn_state INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE model_error_episodes;
