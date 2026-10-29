-- +goose Up
-- Review no longer compares a Reviewer with author launches, so the launch
-- start HEAD has no reader.
ALTER TABLE task_launch_identities DROP COLUMN start_head;

-- +goose Down
ALTER TABLE task_launch_identities ADD COLUMN start_head TEXT NOT NULL DEFAULT '';
