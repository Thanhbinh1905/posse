-- +goose Up
ALTER TABLE member_pr_poll_state ADD COLUMN forge_probe_failures INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE member_pr_poll_state DROP COLUMN forge_probe_failures;
