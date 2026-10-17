-- +goose Up
-- A no-mistakes review receipt that qualified a Ship candidate. The pipeline
-- owns every field except receipt_sha256, which Posse computes over the exact
-- bytes it read. Rows are insert-only evidence of what a merge relied on.
CREATE TABLE review_receipts (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository TEXT NOT NULL,
    run_id TEXT NOT NULL,
    head_sha TEXT NOT NULL CHECK (length(head_sha) = 40 OR length(head_sha) = 64),
    review_status TEXT NOT NULL,
    reviewer_profile TEXT NOT NULL,
    reviewer_model TEXT NOT NULL,
    report_sha256 TEXT NOT NULL CHECK (length(report_sha256) = 64),
    receipt_sha256 TEXT NOT NULL CHECK (length(receipt_sha256) = 64),
    observed_at INTEGER NOT NULL,
    PRIMARY KEY(task_id, receipt_sha256)
);
CREATE TRIGGER review_receipts_immutable BEFORE UPDATE ON review_receipts
BEGIN SELECT RAISE(ABORT, 'review receipt is immutable'); END;

-- +goose Down
DROP TRIGGER review_receipts_immutable;
DROP TABLE review_receipts;
