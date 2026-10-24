-- +goose Up
ALTER TABLE pr_body_markers ADD COLUMN validation_digest TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE pr_body_markers DROP COLUMN validation_digest;
