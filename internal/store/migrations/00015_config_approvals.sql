-- +goose Up
CREATE TABLE config_approvals (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('set', 'unset')),
    value TEXT,
    user_quote TEXT NOT NULL CHECK (length(trim(user_quote)) > 0),
    at INTEGER NOT NULL,
    CHECK ((action = 'set' AND value IS NOT NULL) OR (action = 'unset' AND value IS NULL))
);

-- +goose Down
DROP TABLE config_approvals;
