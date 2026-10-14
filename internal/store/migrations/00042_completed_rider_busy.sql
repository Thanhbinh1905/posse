-- +goose Up
CREATE TABLE completed_rider_busy (
    task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
    transition_id INTEGER NOT NULL REFERENCES transitions(id),
    pane_id TEXT NOT NULL,
    server_started_at TEXT NOT NULL,
    busy_since INTEGER NOT NULL
);

-- +goose Down
DROP TABLE completed_rider_busy;
