-- +goose Up
-- Existing queued messages retain the wait-for-idle behavior after upgrade.
ALTER TABLE messages ADD COLUMN wait_for_idle INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE messages DROP COLUMN wait_for_idle;
