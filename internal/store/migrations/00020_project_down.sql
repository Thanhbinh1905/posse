-- +goose Up
ALTER TABLE projects ADD COLUMN down_at INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE projects DROP COLUMN down_at;
