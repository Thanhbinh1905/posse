-- +goose Up
CREATE TABLE IF NOT EXISTS greenhouse_review_requests (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL,
    requested_head TEXT NOT NULL CHECK (length(requested_head) IN (40, 64)),
    provider TEXT NOT NULL CHECK (provider = 'greenhouse'),
    requested_at INTEGER NOT NULL,
    settled_at INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('verdict', 'refused', 'failed', 'head_mismatch')),
    verdict_id INTEGER REFERENCES verdicts(id),
    grant_id TEXT,
    CHECK (outcome <> 'verdict' OR verdict_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS greenhouse_review_requests_project_idx ON greenhouse_review_requests(project_id, requested_at);
CREATE TRIGGER IF NOT EXISTS greenhouse_review_requests_immutable BEFORE UPDATE ON greenhouse_review_requests
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is immutable'); END;
CREATE TRIGGER IF NOT EXISTS greenhouse_review_requests_no_delete BEFORE DELETE ON greenhouse_review_requests
BEGIN SELECT RAISE(ABORT, 'Greenhouse review request is insert-only'); END;

-- +goose Down
DROP TRIGGER greenhouse_review_requests_no_delete;
DROP TRIGGER greenhouse_review_requests_immutable;
DROP INDEX greenhouse_review_requests_project_idx;
DROP TABLE greenhouse_review_requests;
