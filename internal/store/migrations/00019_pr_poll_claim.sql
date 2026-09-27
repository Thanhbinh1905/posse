-- +goose Up
ALTER TABLE project_watch_state ADD COLUMN pr_poll_claim_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_watch_state ADD COLUMN pr_poll_claim_token TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE project_watch_state DROP COLUMN pr_poll_claim_token;
ALTER TABLE project_watch_state DROP COLUMN pr_poll_claim_until;
