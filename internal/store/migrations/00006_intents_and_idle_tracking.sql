-- +goose Up
ALTER TABLE tasks ADD COLUMN idle_since INTEGER NOT NULL DEFAULT 0;

CREATE TABLE intents (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    command TEXT NOT NULL,
    step TEXT NOT NULL,
    process_id INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    payload_json TEXT NOT NULL DEFAULT '{}',
    UNIQUE(task_id)
);

CREATE INDEX intents_updated_at_idx ON intents(updated_at);

-- +goose Down
DROP INDEX intents_updated_at_idx;
DROP TABLE intents;
ALTER TABLE tasks DROP COLUMN idle_since;
