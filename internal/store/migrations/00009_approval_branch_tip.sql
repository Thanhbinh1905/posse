-- +goose Up
ALTER TABLE approvals ADD COLUMN branch_sha TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE approvals DROP COLUMN branch_sha;
