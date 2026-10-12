-- +goose Up
ALTER TABLE projects ADD COLUMN project_uuid TEXT NOT NULL DEFAULT '';
UPDATE projects SET project_uuid = lower(hex(randomblob(16))) WHERE project_uuid = '';
CREATE UNIQUE INDEX projects_project_uuid_idx ON projects(project_uuid);

-- +goose Down
DROP INDEX projects_project_uuid_idx;
ALTER TABLE projects DROP COLUMN project_uuid;
