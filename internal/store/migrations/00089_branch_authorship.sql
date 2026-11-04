-- +goose Up
CREATE TABLE branch_authorship (
    repository TEXT NOT NULL,
    branch TEXT NOT NULL,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    generation INTEGER NOT NULL CHECK (generation > 0),
    head_sha TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (repository, branch)
);

CREATE INDEX branch_authorship_task_idx ON branch_authorship(task_id);

CREATE TABLE branch_author_invalidated_verdicts (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    change_task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    invalidated_at INTEGER NOT NULL
);
CREATE INDEX branch_author_invalidated_verdicts_change_idx ON branch_author_invalidated_verdicts(change_task_id);

DROP TRIGGER review_rounds_terminal_status_immutable;
CREATE TRIGGER review_rounds_terminal_status_immutable
BEFORE UPDATE OF status, verdict ON review_rounds
WHEN OLD.status = 'void'
    OR (OLD.status = 'verdict' AND NOT (NEW.status = 'void' AND NEW.verdict IS NULL))
    OR NEW.status NOT IN ('verdict', 'void')
    OR (NEW.status = 'verdict' AND NEW.verdict IS NULL)
    OR (NEW.status = 'void' AND NEW.verdict IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'review round status is terminal'); END;

-- +goose Down
DROP TRIGGER review_rounds_terminal_status_immutable;
CREATE TRIGGER review_rounds_terminal_status_immutable
BEFORE UPDATE OF status, verdict ON review_rounds
WHEN OLD.status <> 'admitted'
    OR NEW.status NOT IN ('verdict', 'void')
    OR (NEW.status = 'verdict' AND NEW.verdict IS NULL)
    OR (NEW.status = 'void' AND NEW.verdict IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'review round status is terminal'); END;
DROP TABLE branch_author_invalidated_verdicts;
DROP TABLE branch_authorship;
