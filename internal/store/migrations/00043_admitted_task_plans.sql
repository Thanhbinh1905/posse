-- +goose Up
ALTER TABLE tasks ADD COLUMN plan_json TEXT NOT NULL DEFAULT '';
ALTER TABLE task_launch_identities ADD COLUMN plan_revision INTEGER NOT NULL DEFAULT 0;

-- Empty plan_json deliberately keeps existing Tasks on their legacy path.
CREATE TABLE task_plan_revisions (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (revision > 0),
    plan_json TEXT NOT NULL,
    PRIMARY KEY(task_id, revision)
);

-- +goose Down
DROP TABLE task_plan_revisions;
ALTER TABLE task_launch_identities DROP COLUMN plan_revision;
ALTER TABLE tasks DROP COLUMN plan_json;
