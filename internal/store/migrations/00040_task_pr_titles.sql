-- +goose Up
ALTER TABLE tasks ADD COLUMN pr_title TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN pr_title;
