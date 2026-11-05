-- +goose Up
CREATE TABLE task_adoptions (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    source_task_id INTEGER NOT NULL UNIQUE REFERENCES tasks(id),
    repository TEXT NOT NULL,
    branch TEXT NOT NULL,
    pr_url TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    generation INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE task_adoptions;
