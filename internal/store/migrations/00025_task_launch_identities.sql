-- +goose Up
CREATE TABLE task_launch_identities (
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    launch_number INTEGER NOT NULL CHECK (launch_number > 0),
    profile_name TEXT NOT NULL DEFAULT '',
    configured_model TEXT NOT NULL DEFAULT '',
    model_known INTEGER NOT NULL DEFAULT 0 CHECK (model_known IN (0, 1)),
    PRIMARY KEY (task_id, launch_number)
);

-- +goose Down
DROP TABLE task_launch_identities;
