-- +goose Up
CREATE TABLE validation_stage_results (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    candidate_id INTEGER NOT NULL REFERENCES validation_candidates(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    stage TEXT NOT NULL CHECK (stage IN ('test', 'docs', 'lint')),
    candidate_sha TEXT NOT NULL CHECK (length(candidate_sha) IN (40, 64)),
    target_sha TEXT NOT NULL CHECK (length(target_sha) IN (40, 64)),
    config_sha256 TEXT NOT NULL CHECK (length(config_sha256) = 64),
    status TEXT NOT NULL CHECK (status IN ('passed', 'invalidated')),
    summary TEXT NOT NULL,
    evidence TEXT NOT NULL,
    coverage TEXT NOT NULL DEFAULT '',
    invalidated_by_sha TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (candidate_id, stage, candidate_sha),
    CHECK (invalidated_by_sha = '' OR length(invalidated_by_sha) IN (40, 64))
);
CREATE INDEX validation_stage_results_candidate_idx ON validation_stage_results(candidate_id, stage, created_at);

-- +goose Down
DROP INDEX validation_stage_results_candidate_idx;
DROP TABLE validation_stage_results;
