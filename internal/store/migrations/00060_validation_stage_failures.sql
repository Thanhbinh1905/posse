-- +goose Up
DROP INDEX validation_stage_results_candidate_idx;
ALTER TABLE validation_stage_results RENAME TO validation_stage_results_old;
CREATE TABLE validation_stage_results (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    candidate_id INTEGER NOT NULL REFERENCES validation_candidates(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    stage TEXT NOT NULL CHECK (stage IN ('test', 'docs', 'lint')),
    candidate_sha TEXT NOT NULL CHECK (length(candidate_sha) IN (40, 64)),
    target_sha TEXT NOT NULL CHECK (length(target_sha) IN (40, 64)),
    config_sha256 TEXT NOT NULL CHECK (length(config_sha256) = 64),
    status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'invalidated')),
    summary TEXT NOT NULL,
    evidence TEXT NOT NULL,
    coverage TEXT NOT NULL DEFAULT '',
    invalidated_by_sha TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (candidate_id, stage, candidate_sha),
    CHECK (invalidated_by_sha = '' OR length(invalidated_by_sha) IN (40, 64))
);
INSERT INTO validation_stage_results
    (id, task_id, candidate_id, repo, stage, candidate_sha, target_sha, config_sha256, status, summary, evidence, coverage, invalidated_by_sha, created_at)
SELECT id, task_id, candidate_id, repo, stage, candidate_sha, target_sha, config_sha256, status, summary, evidence, coverage, invalidated_by_sha, created_at
FROM validation_stage_results_old;
DROP TABLE validation_stage_results_old;
CREATE INDEX validation_stage_results_candidate_idx ON validation_stage_results(candidate_id, stage, created_at);

-- +goose Down
DROP INDEX validation_stage_results_candidate_idx;
ALTER TABLE validation_stage_results RENAME TO validation_stage_results_new;
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
INSERT INTO validation_stage_results
    (id, task_id, candidate_id, repo, stage, candidate_sha, target_sha, config_sha256, status, summary, evidence, coverage, invalidated_by_sha, created_at)
SELECT id, task_id, candidate_id, repo, stage, candidate_sha, target_sha, config_sha256, status, summary, evidence, coverage, invalidated_by_sha, created_at
FROM validation_stage_results_new
WHERE status IN ('passed', 'invalidated');
DROP TABLE validation_stage_results_new;
CREATE INDEX validation_stage_results_candidate_idx ON validation_stage_results(candidate_id, stage, created_at);
