-- +goose Up
ALTER TABLE tasks ADD COLUMN pane_shell_identity TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN pane_shell_identity;
