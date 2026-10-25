-- +goose Up
-- One record per Worker launch whose harness reported an exhausted quota or
-- usage limit. The primary key lets exactly one observer act on that launch.
CREATE TABLE quota_exhaustions (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    launch INTEGER NOT NULL,
    agent TEXT NOT NULL,
    profile TEXT NOT NULL,
    line TEXT NOT NULL,
    resets TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    fallback TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    owner_pid INTEGER NOT NULL DEFAULT 0,
    observed_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, launch)
);

-- +goose Down
DROP TABLE quota_exhaustions;
