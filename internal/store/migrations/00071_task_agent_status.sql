-- +goose Up
-- The Herdr agent status last observed for a live Task, and when Posse first
-- observed it. Display only: the Task state stays authoritative.
ALTER TABLE tasks ADD COLUMN agent_status TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN agent_status_since INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE tasks DROP COLUMN agent_status_since;
ALTER TABLE tasks DROP COLUMN agent_status;
