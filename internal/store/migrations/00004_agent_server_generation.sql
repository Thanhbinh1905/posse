-- +goose Up
ALTER TABLE tasks ADD COLUMN agent_server_started_at TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN agent_server_started_at;
