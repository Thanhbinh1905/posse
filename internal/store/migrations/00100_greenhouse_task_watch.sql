-- +goose Up
CREATE TABLE IF NOT EXISTS greenhouse_task_watches (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    run_id TEXT NOT NULL,
    status TEXT NOT NULL,
    stage TEXT NOT NULL,
    run_observed_at INTEGER NOT NULL,
    stage_observed_at INTEGER NOT NULL,
    run_started_at INTEGER NOT NULL DEFAULT 0,
    stage_started_at INTEGER NOT NULL DEFAULT 0,
    stage_finished_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    pane_id TEXT NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE greenhouse_task_watches;
