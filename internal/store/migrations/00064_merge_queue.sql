-- +goose Up
CREATE TABLE merge_queue_entries (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    repo TEXT NOT NULL DEFAULT '',
    target_branch TEXT NOT NULL,
    entered_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    dequeued_at INTEGER NOT NULL DEFAULT 0,
    dequeue_reason TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX merge_queue_entries_live_idx ON merge_queue_entries(task_id, repo) WHERE dequeued_at = 0;
CREATE INDEX merge_queue_entries_target_idx ON merge_queue_entries(project_id, repo, target_branch, dequeued_at);

-- +goose Down
DROP INDEX merge_queue_entries_target_idx;
DROP INDEX merge_queue_entries_live_idx;
DROP TABLE merge_queue_entries;
