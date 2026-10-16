-- +goose Up
ALTER TABLE tasks ADD COLUMN report_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN report_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN report_manifest_json TEXT NOT NULL DEFAULT '';

CREATE TABLE report_reviews (
    id INTEGER PRIMARY KEY,
    report_task_id INTEGER NOT NULL REFERENCES tasks(id),
    review_task_id INTEGER NOT NULL REFERENCES tasks(id),
    report_digest TEXT NOT NULL,
    verdict TEXT NOT NULL CHECK (verdict IN ('pass', 'changes_requested', 'inconclusive')),
    reviewer_launch INTEGER NOT NULL CHECK (reviewer_launch > 0),
    recorded_at INTEGER NOT NULL,
    UNIQUE(report_task_id, review_task_id, report_digest, reviewer_launch)
);
CREATE INDEX report_reviews_target_idx ON report_reviews(report_task_id, report_digest, verdict, recorded_at);

CREATE TABLE report_acceptances (
    report_task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    report_digest TEXT NOT NULL,
    accepted_by TEXT NOT NULL CHECK (accepted_by = 'user'),
    recorded_by TEXT NOT NULL CHECK (recorded_by IN ('user', 'lead')),
    provenance TEXT NOT NULL CHECK (provenance IN ('user-command', 'lead-quote')),
    user_quote TEXT NOT NULL DEFAULT '',
    accepted_at INTEGER NOT NULL,
    CHECK (
        (recorded_by = 'user' AND provenance = 'user-command') OR
        (recorded_by = 'lead' AND provenance = 'lead-quote' AND length(trim(user_quote)) > 0)
    )
);

-- +goose Down
DROP TABLE report_acceptances;
DROP INDEX report_reviews_target_idx;
DROP TABLE report_reviews;
ALTER TABLE tasks DROP COLUMN report_manifest_json;
ALTER TABLE tasks DROP COLUMN report_digest;
ALTER TABLE tasks DROP COLUMN report_kind;
