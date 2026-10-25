-- +goose Up
ALTER TABLE completed_rider_busy ADD COLUMN baseline TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE completed_rider_busy DROP COLUMN baseline;
