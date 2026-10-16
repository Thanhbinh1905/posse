-- +goose Up
-- A Review Task is bound at admission to the exact commit of each repository
-- in the completed Ship candidate. Targets can be inserted but never changed.
CREATE TABLE review_targets (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repository TEXT NOT NULL,
    commit_sha TEXT NOT NULL CHECK (length(commit_sha) = 40 OR length(commit_sha) = 64),
    PRIMARY KEY(task_id, repository)
);
CREATE TRIGGER review_targets_immutable BEFORE UPDATE ON review_targets
BEGIN SELECT RAISE(ABORT, 'review target is immutable'); END;

-- One verdict per Review Task, written in the same transaction as its done
-- Signal. signal_id is informational: rebuilds restore verdicts without Signals.
CREATE TABLE review_verdicts (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    signal_id INTEGER NOT NULL,
    verdict TEXT NOT NULL CHECK (verdict IN ('pass', 'changes_requested', 'inconclusive')),
    report_sha256 TEXT NOT NULL CHECK (length(report_sha256) = 64),
    targets_json TEXT NOT NULL,
    launch_number INTEGER NOT NULL CHECK (launch_number > 0),
    profile_name TEXT NOT NULL,
    configured_model TEXT NOT NULL,
    model_known INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TRIGGER review_verdicts_immutable BEFORE UPDATE ON review_verdicts
BEGIN SELECT RAISE(ABORT, 'review verdict is immutable'); END;

-- +goose Down
DROP TRIGGER review_verdicts_immutable;
DROP TABLE review_verdicts;
DROP TRIGGER review_targets_immutable;
DROP TABLE review_targets;
