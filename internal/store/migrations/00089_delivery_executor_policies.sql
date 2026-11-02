-- +goose Up
DROP INDEX delivery_operations_subject_idx;
ALTER TABLE delivery_operations RENAME TO delivery_operations_old;
CREATE TABLE delivery_operations (
    id INTEGER PRIMARY KEY,
    delivery_id INTEGER NOT NULL REFERENCES deliveries(id),
    operation TEXT NOT NULL CHECK (operation IN ('inspect', 'reply', 'refresh', 'rerun', 'executor_policy')),
    actor TEXT NOT NULL CHECK (actor IN ('lead', 'user')),
    actor_pane TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    subject TEXT NOT NULL DEFAULT '',
    baseline TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL CHECK (outcome IN ('pending', 'succeeded', 'failed', 'uncertain')),
    detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
INSERT INTO delivery_operations SELECT * FROM delivery_operations_old;
DROP TABLE delivery_operations_old;
CREATE INDEX delivery_operations_subject_idx ON delivery_operations(delivery_id, operation, head_sha, subject);

-- +goose Down
DROP INDEX delivery_operations_subject_idx;
ALTER TABLE delivery_operations RENAME TO delivery_operations_new;
CREATE TABLE delivery_operations (
    id INTEGER PRIMARY KEY,
    delivery_id INTEGER NOT NULL REFERENCES deliveries(id),
    operation TEXT NOT NULL CHECK (operation IN ('inspect', 'reply', 'refresh', 'rerun')),
    actor TEXT NOT NULL CHECK (actor IN ('lead', 'user')),
    actor_pane TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    subject TEXT NOT NULL DEFAULT '',
    baseline TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL CHECK (outcome IN ('pending', 'succeeded', 'failed', 'uncertain')),
    detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
INSERT INTO delivery_operations SELECT * FROM delivery_operations_new WHERE operation <> 'executor_policy';
DROP TABLE delivery_operations_new;
CREATE INDEX delivery_operations_subject_idx ON delivery_operations(delivery_id, operation, head_sha, subject);
