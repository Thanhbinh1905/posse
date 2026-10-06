-- +goose Up
ALTER TABLE repo_watch_state ADD COLUMN checkout_checked_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE repo_watch_state ADD COLUMN checkout_status TEXT NOT NULL DEFAULT '';
ALTER TABLE repo_watch_state ADD COLUMN checkout_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE repo_watch_state DROP COLUMN checkout_reason;
ALTER TABLE repo_watch_state DROP COLUMN checkout_status;
ALTER TABLE repo_watch_state DROP COLUMN checkout_checked_at;
