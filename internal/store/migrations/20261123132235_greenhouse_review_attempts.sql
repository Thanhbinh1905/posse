-- +goose Up
ALTER TABLE greenhouse_review_requests ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0);
UPDATE greenhouse_review_requests SET failure_count=1 WHERE outcome='failed';

-- +goose Down
ALTER TABLE greenhouse_review_requests DROP COLUMN failure_count;
