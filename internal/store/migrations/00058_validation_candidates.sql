-- +goose Up
CREATE TABLE validation_candidates (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL CHECK (generation > 0),
    submitted_sha TEXT NOT NULL CHECK (length(submitted_sha) IN (40, 64)),
    candidate_sha TEXT NOT NULL CHECK (length(candidate_sha) IN (40, 64)),
    target_sha TEXT NOT NULL CHECK (length(target_sha) IN (40, 64)),
    expected_remote_sha TEXT NOT NULL DEFAULT '' CHECK (expected_remote_sha = '' OR length(expected_remote_sha) IN (40, 64)),
    observed_author_sha TEXT NOT NULL DEFAULT '',
    ref_name TEXT NOT NULL,
    worktree_path TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('preparing', 'active', 'stale', 'promoting', 'promoted', 'superseded', 'discarded')),
    promoted_sha TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL,
    verification TEXT NOT NULL DEFAULT '',
    proof TEXT NOT NULL DEFAULT '',
    risk TEXT NOT NULL DEFAULT '',
    refresh INTEGER NOT NULL DEFAULT 0 CHECK (refresh IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (task_id, repo, generation),
    CHECK (state <> 'promoted' OR promoted_sha = candidate_sha)
);
CREATE UNIQUE INDEX validation_candidates_one_live_idx ON validation_candidates(task_id, repo)
    WHERE state IN ('preparing', 'active', 'stale', 'promoting');
CREATE INDEX validation_candidates_task_idx ON validation_candidates(task_id, repo, generation);
CREATE TRIGGER validation_candidates_submission_immutable
BEFORE UPDATE OF task_id, repo, generation, submitted_sha, target_sha, expected_remote_sha,
    summary, verification, proof, risk, refresh, created_at ON validation_candidates
BEGIN SELECT RAISE(ABORT, 'validation candidate submission is immutable'); END;

-- +goose Down
DROP TRIGGER validation_candidates_submission_immutable;
DROP INDEX validation_candidates_task_idx;
DROP INDEX validation_candidates_one_live_idx;
DROP TABLE validation_candidates;
