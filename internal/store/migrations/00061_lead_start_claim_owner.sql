-- +goose Up
ALTER TABLE lead_start_claims ADD COLUMN holder TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE lead_start_claims ADD COLUMN pid INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE lead_start_claims DROP COLUMN pid;
ALTER TABLE lead_start_claims DROP COLUMN holder;
