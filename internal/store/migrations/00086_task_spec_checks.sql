-- +goose Up
-- Tasks whose spec_ready_to_deliver check has completed once after their PR merged.
CREATE TABLE task_spec_checks (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    checked_at INTEGER NOT NULL
);

INSERT INTO task_spec_checks(task_id, checked_at)
SELECT id, 0 FROM tasks WHERE state IN ('merged', 'reported', 'torn-down');

-- +goose Down
DROP TABLE task_spec_checks;
