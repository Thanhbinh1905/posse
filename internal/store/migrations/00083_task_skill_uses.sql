-- +goose Up
-- Skills a Task's Worker invoked, as reported by its harness hook.
CREATE TABLE task_skill_uses (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    skill TEXT NOT NULL,
    uses INTEGER NOT NULL DEFAULT 1,
    first_at INTEGER NOT NULL,
    last_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, skill)
);

-- +goose Down
DROP TABLE task_skill_uses;
