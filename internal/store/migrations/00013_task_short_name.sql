-- +goose Up
ALTER TABLE tasks ADD COLUMN short_name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN short_name;
