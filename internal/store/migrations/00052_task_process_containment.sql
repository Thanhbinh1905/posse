-- +goose Up
ALTER TABLE tasks ADD COLUMN containment_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN containment_path TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN containment_marker TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN containment_marker;
ALTER TABLE tasks DROP COLUMN containment_path;
ALTER TABLE tasks DROP COLUMN containment_mode;
