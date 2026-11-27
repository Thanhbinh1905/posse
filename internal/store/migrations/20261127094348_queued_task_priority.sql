-- +goose Up
ALTER TABLE tasks ADD COLUMN queue_priority INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE tasks DROP COLUMN queue_priority;
