-- +goose Up
CREATE TABLE mounts (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    n INTEGER NOT NULL,
    path TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL,
    task_id INTEGER REFERENCES tasks(id),
    acquired_at INTEGER NOT NULL DEFAULT 0,
    released_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE(project_id, n)
);

ALTER TABLE projects ADD COLUMN lead_launches INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN mount_id INTEGER REFERENCES mounts(id);
ALTER TABLE tasks ADD COLUMN launches INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN gated_sha TEXT NOT NULL DEFAULT '';

CREATE INDEX mounts_project_state_idx ON mounts(project_id, state, n);
CREATE UNIQUE INDEX notices_worker_exited_once ON notices(task_id) WHERE kind = 'worker_exited';

-- +goose Down
DROP INDEX notices_worker_exited_once;
DROP INDEX mounts_project_state_idx;
ALTER TABLE tasks DROP COLUMN gated_sha;
ALTER TABLE tasks DROP COLUMN launches;
ALTER TABLE tasks DROP COLUMN mount_id;
ALTER TABLE projects DROP COLUMN lead_launches;
DROP TABLE mounts;
