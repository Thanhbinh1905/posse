-- +goose Up
-- The PR description fields stay editable until promotion so a wording miss
-- does not force a new candidate commit.
DROP TRIGGER validation_candidates_submission_immutable;
-- +goose StatementBegin
CREATE TRIGGER validation_candidates_submission_immutable
BEFORE UPDATE OF task_id, repo, generation, submitted_sha, target_sha, expected_remote_sha,
    refresh, created_at ON validation_candidates
BEGIN SELECT RAISE(ABORT, 'validation candidate submission is immutable'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER validation_candidates_submission_immutable;
-- +goose StatementBegin
CREATE TRIGGER validation_candidates_submission_immutable
BEFORE UPDATE OF task_id, repo, generation, submitted_sha, target_sha, expected_remote_sha,
    summary, verification, proof, risk, refresh, created_at ON validation_candidates
BEGIN SELECT RAISE(ABORT, 'validation candidate submission is immutable'); END;
-- +goose StatementEnd
