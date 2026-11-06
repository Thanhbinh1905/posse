-- +goose Up
ALTER TABLE decisions ADD COLUMN answer_actor TEXT NOT NULL DEFAULT 'user';
ALTER TABLE decisions ADD COLUMN answer_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE review_dispositions ADD COLUMN actor TEXT NOT NULL DEFAULT 'user';
ALTER TABLE review_dispositions ADD COLUMN reason TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS review_cap_final_review_once ON notices(task_id) WHERE kind='review_cap_final_review';

-- +goose Down
DROP INDEX review_cap_final_review_once;
ALTER TABLE decisions DROP COLUMN answer_reason;
ALTER TABLE review_dispositions DROP COLUMN reason;
ALTER TABLE review_dispositions DROP COLUMN actor;
ALTER TABLE decisions DROP COLUMN answer_actor;
