-- +goose Up
ALTER TABLE notices ADD COLUMN command_notified_at INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE notices DROP COLUMN command_notified_at;
