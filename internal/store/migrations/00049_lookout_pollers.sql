-- +goose Up
CREATE TABLE project_lookout_pollers (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    pid INTEGER NOT NULL,
    token TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    heartbeat_at INTEGER NOT NULL,
    executable_path TEXT NOT NULL,
    executable_device INTEGER NOT NULL,
    executable_inode INTEGER NOT NULL,
    failure_notice_raised INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE project_lookout_pollers;
