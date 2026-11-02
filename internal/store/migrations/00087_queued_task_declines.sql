-- +goose Up
-- Why queue refill last declined a queued Task after its network admission
-- checks, so status reads report it without repeating those checks.
CREATE TABLE queued_task_declines (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    checked_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE queued_task_declines;
