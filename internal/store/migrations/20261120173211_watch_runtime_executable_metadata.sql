-- +goose Up
ALTER TABLE project_lookout_pollers ADD COLUMN executable_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_lookout_pollers ADD COLUMN executable_modified_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_lookout_pollers ADD COLUMN executable_version TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE project_lookout_pollers DROP COLUMN executable_version;
ALTER TABLE project_lookout_pollers DROP COLUMN executable_modified_at;
ALTER TABLE project_lookout_pollers DROP COLUMN executable_size;
