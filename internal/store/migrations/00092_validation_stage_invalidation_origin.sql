-- +goose Up
ALTER TABLE validation_stage_results
    ADD COLUMN invalidated_from_status TEXT NOT NULL DEFAULT ''
    CHECK (invalidated_from_status IN ('', 'passed', 'failed'));

-- +goose Down
ALTER TABLE validation_stage_results DROP COLUMN invalidated_from_status;
