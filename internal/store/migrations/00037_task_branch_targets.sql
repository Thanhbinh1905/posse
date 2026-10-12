-- +goose Up
ALTER TABLE tasks ADD COLUMN target_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE task_repos ADD COLUMN target_branch TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE task_repos DROP COLUMN target_branch;
ALTER TABLE tasks DROP COLUMN target_branch;
