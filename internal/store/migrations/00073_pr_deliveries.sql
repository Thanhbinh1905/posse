-- +goose Up
-- A delivery is the forge PR/MR of a Task or workspace Member. The Lead is its
-- operational steward; the Task's recorded launches remain its authors.
CREATE TABLE deliveries (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    repo TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL CHECK (pr_url <> ''),
    steward TEXT NOT NULL DEFAULT 'lead' CHECK (steward IN ('lead')),
    origin TEXT NOT NULL CHECK (origin IN ('publish', 'legacy')),
    created_at INTEGER NOT NULL,
    UNIQUE (task_id, repo, pr_url)
);

-- Every steward forge operation records its actor, the PR head it acted on
-- and its outcome. A pending or uncertain operation is observed on the forge
-- before the same operation is attempted again.
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
CREATE INDEX delivery_operations_subject_idx ON delivery_operations(delivery_id, operation, head_sha, subject);

-- confirmed_at is when the watch last read an observation's state from the
-- forge; an unchanged poll updates it instead of inserting a row.
ALTER TABLE pr_observations ADD COLUMN confirmed_at INTEGER NOT NULL DEFAULT 0;
UPDATE pr_observations SET confirmed_at=observed_at;

-- PR URLs recorded before deliveries existed become legacy deliveries.
INSERT INTO deliveries(task_id, repo, pr_url, steward, origin, created_at)
SELECT id, '', pr_url, 'lead', 'legacy', updated_at FROM tasks WHERE pr_url <> '';
INSERT OR IGNORE INTO deliveries(task_id, repo, pr_url, steward, origin, created_at)
SELECT task_id, repo, pr_url, 'lead', 'legacy', updated_at FROM task_repos WHERE pr_url <> '';

-- +goose Down
ALTER TABLE pr_observations DROP COLUMN confirmed_at;
DROP INDEX delivery_operations_subject_idx;
DROP TABLE delivery_operations;
DROP TABLE deliveries;
