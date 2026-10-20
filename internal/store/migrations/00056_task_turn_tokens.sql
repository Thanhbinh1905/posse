-- +goose Up
CREATE TABLE task_turn_tokens (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    token TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE task_turn_tokens;
