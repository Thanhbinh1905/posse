-- +goose Up
-- +goose StatementBegin
CREATE TABLE config_approvals_with_move (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('set', 'unset', 'move')),
    value TEXT,
    user_quote TEXT NOT NULL CHECK (length(trim(user_quote)) > 0),
    at INTEGER NOT NULL,
    CHECK ((action IN ('set', 'move') AND value IS NOT NULL) OR (action = 'unset' AND value IS NULL))
);

INSERT INTO config_approvals_with_move(id, project_id, key, action, value, user_quote, at)
SELECT id, project_id, key, action, value, user_quote, at FROM config_approvals;

DROP TABLE config_approvals;
ALTER TABLE config_approvals_with_move RENAME TO config_approvals;
-- +goose StatementEnd

-- +goose Down
-- The old schema cannot represent move approvals. Copying them into its CHECK
-- constraint fails and rolls the migration back instead of degrading the audit.
-- +goose StatementBegin
CREATE TABLE config_approvals_without_move (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('set', 'unset')),
    value TEXT,
    user_quote TEXT NOT NULL CHECK (length(trim(user_quote)) > 0),
    at INTEGER NOT NULL,
    CHECK ((action = 'set' AND value IS NOT NULL) OR (action = 'unset' AND value IS NULL))
);

INSERT INTO config_approvals_without_move(id, project_id, key, action, value, user_quote, at)
SELECT id, project_id, key, action, value, user_quote, at FROM config_approvals;

DROP TABLE config_approvals;
ALTER TABLE config_approvals_without_move RENAME TO config_approvals;
-- +goose StatementEnd
