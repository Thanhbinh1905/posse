-- +goose Up
ALTER TABLE task_plan_input_migration ADD COLUMN tasks_done INTEGER NOT NULL DEFAULT 0 CHECK (tasks_done IN (0,1));
ALTER TABLE task_plan_input_migration ADD COLUMN revisions_done INTEGER NOT NULL DEFAULT 0 CHECK (revisions_done IN (0,1));
ALTER TABLE task_plan_input_migration ADD COLUMN task_cursor INTEGER NOT NULL DEFAULT 0;
ALTER TABLE task_plan_input_migration ADD COLUMN revision_task_cursor INTEGER NOT NULL DEFAULT 0;
ALTER TABLE task_plan_input_migration ADD COLUMN revision_cursor INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE task_plan_input_migration DROP COLUMN revision_cursor;
ALTER TABLE task_plan_input_migration DROP COLUMN revision_task_cursor;
ALTER TABLE task_plan_input_migration DROP COLUMN task_cursor;
ALTER TABLE task_plan_input_migration DROP COLUMN revisions_done;
ALTER TABLE task_plan_input_migration DROP COLUMN tasks_done;
