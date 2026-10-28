-- +goose Up
-- The last Project PR poll failure, with the forge tool's stderr, so a steward
-- command that could not observe the PR names why.
ALTER TABLE project_watch_state ADD COLUMN pr_poll_last_error TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE project_watch_state DROP COLUMN pr_poll_last_error;
