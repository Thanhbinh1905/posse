-- +goose Up
ALTER TABLE tasks ADD COLUMN workflow_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN plan_revision INTEGER NOT NULL DEFAULT 0;
CREATE INDEX tasks_project_workflow_idx ON tasks(project_id, workflow_run_id, seq);
CREATE INDEX tasks_project_pane_label_idx ON tasks(project_id, pane_label, seq);
CREATE INDEX tasks_project_seq_idx ON tasks(project_id, seq);

CREATE TABLE task_plan_input_migration (
    id INTEGER PRIMARY KEY CHECK (id=1),
    completed INTEGER NOT NULL DEFAULT 0 CHECK (completed IN (0,1))
);
INSERT INTO task_plan_input_migration(id,completed) VALUES(1,0);

-- +goose Down
DROP TABLE task_plan_input_migration;
DROP INDEX tasks_project_seq_idx;
DROP INDEX tasks_project_pane_label_idx;
DROP INDEX tasks_project_workflow_idx;
ALTER TABLE tasks DROP COLUMN plan_revision;
ALTER TABLE tasks DROP COLUMN workflow_run_id;
