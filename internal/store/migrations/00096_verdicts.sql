-- +goose Up
CREATE TABLE verdicts (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL,
    provider TEXT NOT NULL CHECK (provider IN ('builtin', 'greenhouse')),
    protocol TEXT NOT NULL DEFAULT '',
    grant_id TEXT UNIQUE,
    head_sha TEXT NOT NULL CHECK (length(head_sha) IN (40, 64)),
    target_sha TEXT NOT NULL CHECK (length(target_sha) IN (40, 64)),
    review_base_sha TEXT NOT NULL CHECK (length(review_base_sha) IN (40, 64)),
    verdict TEXT NOT NULL CHECK (verdict IN ('clean', 'findings', 'inconclusive')),
    blocking INTEGER NOT NULL CHECK (blocking >= 0),
    rules_digest TEXT NOT NULL DEFAULT '',
    config_digest TEXT NOT NULL DEFAULT '',
    input_digest TEXT NOT NULL DEFAULT '',
    backend TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    findings_digest TEXT NOT NULL DEFAULT '',
    findings_path TEXT NOT NULL DEFAULT '',
    mode TEXT NOT NULL CHECK (mode IN ('parallel', 'gate')),
    recorded_at INTEGER NOT NULL,
    CHECK (verdict = 'findings' OR blocking = 0)
);
CREATE INDEX verdicts_task_idx ON verdicts(task_id, recorded_at DESC);
CREATE TRIGGER verdicts_immutable BEFORE UPDATE ON verdicts
BEGIN SELECT RAISE(ABORT, 'Verdict is immutable'); END;
CREATE TRIGGER verdicts_no_delete BEFORE DELETE ON verdicts
BEGIN SELECT RAISE(ABORT, 'Verdict is insert-only'); END;

-- +goose Down
DROP TRIGGER verdicts_no_delete;
DROP TRIGGER verdicts_immutable;
DROP INDEX verdicts_task_idx;
DROP TABLE verdicts;
