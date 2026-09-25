-- +goose Up
ALTER TABLE project_runtime ADD COLUMN recovery_generation TEXT NOT NULL DEFAULT '';
ALTER TABLE project_runtime ADD COLUMN recovery_owner_pid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_runtime ADD COLUMN recovery_claimed_at INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE project_runtime DROP COLUMN recovery_claimed_at;
ALTER TABLE project_runtime DROP COLUMN recovery_owner_pid;
ALTER TABLE project_runtime DROP COLUMN recovery_generation;
