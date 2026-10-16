-- +goose Up
CREATE TABLE resource_leases (
    id INTEGER PRIMARY KEY,
    resource_type TEXT NOT NULL CHECK(resource_type IN ('path', 'git_ref')),
    scope_root TEXT NOT NULL DEFAULT '',
    scope_path TEXT NOT NULL,
    owner_project_id INTEGER NOT NULL REFERENCES projects(id),
    owner_task_id INTEGER REFERENCES tasks(id),
    owner TEXT NOT NULL,
    source_key TEXT NOT NULL UNIQUE,
    custody_token TEXT NOT NULL,
    intent_id INTEGER NOT NULL DEFAULT 0,
    mount_id INTEGER REFERENCES mounts(id) ON DELETE CASCADE,
    owner_pid INTEGER NOT NULL DEFAULT 0,
    owner_boot_id TEXT NOT NULL DEFAULT '',
    owner_start_time TEXT NOT NULL DEFAULT '',
    custody TEXT NOT NULL DEFAULT 'held' CHECK(custody IN ('held', 'recovery')),
    acquired_at INTEGER NOT NULL,
    CHECK((resource_type = 'path' AND scope_root = '') OR (resource_type = 'git_ref' AND scope_root <> ''))
);

CREATE INDEX resource_leases_scope_idx ON resource_leases(resource_type, scope_root);
CREATE INDEX resource_leases_owner_idx ON resource_leases(owner_project_id, owner_task_id, custody);
CREATE INDEX resource_leases_intent_idx ON resource_leases(intent_id) WHERE intent_id != 0;

-- +goose Down
DROP TABLE resource_leases;
