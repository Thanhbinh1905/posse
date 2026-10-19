-- +goose Up
ALTER TABLE completed_rider_busy ADD COLUMN turn_ended INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE completed_rider_busy DROP COLUMN turn_ended;
