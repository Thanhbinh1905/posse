-- +goose Up
CREATE TABLE IF NOT EXISTS task_launch_usage (
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    launch INTEGER NOT NULL CHECK (launch > 0),
    session_id TEXT NOT NULL,
    session_kind TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    cost_usd REAL,
    profile TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(task_id, launch, session_id)
);
CREATE INDEX IF NOT EXISTS task_launch_usage_task_idx ON task_launch_usage(task_id, launch);

CREATE TABLE IF NOT EXISTS task_review_usage (
    request_id INTEGER PRIMARY KEY REFERENCES greenhouse_review_requests(id) ON DELETE CASCADE,
    task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    cost_usd REAL,
    usage_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS task_review_usage_task_idx ON task_review_usage(task_id);

-- +goose Down
DROP INDEX IF EXISTS task_review_usage_task_idx;
DROP TABLE IF EXISTS task_review_usage;
DROP INDEX IF EXISTS task_launch_usage_task_idx;
DROP TABLE IF EXISTS task_launch_usage;
