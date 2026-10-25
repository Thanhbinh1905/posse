-- +goose Up
-- A completed Worker observed working after its turn ended has started a new
-- turn. That turn's end decides whether it changed the repository.
ALTER TABLE completed_rider_busy ADD COLUMN resumed INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE completed_rider_busy DROP COLUMN resumed;
