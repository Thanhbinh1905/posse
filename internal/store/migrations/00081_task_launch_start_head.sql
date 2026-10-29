-- +goose Up
-- The HEAD of a clean Task worktree when the launch was allocated, empty when
-- unknown or dirty. Two launches that start on the same clean HEAD show that
-- the earlier one left no commit.
ALTER TABLE task_launch_identities ADD COLUMN start_head TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE task_launch_identities DROP COLUMN start_head;
