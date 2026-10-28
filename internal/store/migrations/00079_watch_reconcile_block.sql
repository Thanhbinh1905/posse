-- +goose Up
-- The watch records why its reconcile pass keeps not completing, so the User
-- gets one Notice per episode and queued Tasks show the real blocker.
ALTER TABLE project_watch_state ADD COLUMN reconcile_blocked_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_watch_state ADD COLUMN reconcile_block_cause TEXT NOT NULL DEFAULT '';
ALTER TABLE project_watch_state ADD COLUMN reconcile_block_notice_raised INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE project_watch_state DROP COLUMN reconcile_block_notice_raised;
ALTER TABLE project_watch_state DROP COLUMN reconcile_block_cause;
ALTER TABLE project_watch_state DROP COLUMN reconcile_blocked_since;
