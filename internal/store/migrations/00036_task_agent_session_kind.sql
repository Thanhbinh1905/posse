-- +goose Up
ALTER TABLE tasks ADD COLUMN agent_session_kind TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN agent_session_kind;
