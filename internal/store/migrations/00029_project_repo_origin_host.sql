-- +goose Up
ALTER TABLE project_repos ADD COLUMN origin_host TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE project_repos DROP COLUMN origin_host;
