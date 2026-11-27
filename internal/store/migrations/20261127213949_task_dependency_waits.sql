-- +goose Up
CREATE TABLE task_dependency_waits (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    dependency_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    CHECK (task_id <> dependency_id)
);

-- +goose StatementBegin
CREATE TRIGGER clear_task_dependency_wait
AFTER UPDATE OF state ON tasks
WHEN OLD.state = 'blocked' AND NEW.state <> 'blocked'
BEGIN
    DELETE FROM task_dependency_waits WHERE task_id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER clear_task_dependency_wait;
UPDATE tasks SET state = 'working'
WHERE state = 'blocked' AND id IN (SELECT task_id FROM task_dependency_waits);
DROP TABLE task_dependency_waits;
