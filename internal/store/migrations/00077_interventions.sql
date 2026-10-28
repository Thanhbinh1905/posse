-- +goose Up
-- One row per Lead or Posse action that steers a Worker or the Lead, so the
-- baselines can count interventions the other tables do not keep.
CREATE TABLE interventions (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id INTEGER REFERENCES tasks(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);

CREATE INDEX interventions_project_kind ON interventions(project_id, kind);

-- +goose Down
DROP TABLE interventions;
