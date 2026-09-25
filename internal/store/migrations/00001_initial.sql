-- +goose Up
CREATE TABLE projects (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    root TEXT NOT NULL UNIQUE,
    default_branch TEXT NOT NULL DEFAULT '',
    herdr_workspace_id TEXT NOT NULL DEFAULT '',
    lead_pane_id TEXT NOT NULL DEFAULT '',
    lead_label TEXT NOT NULL DEFAULT '',
    lead_absent_since INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'active',
    created_at INTEGER NOT NULL,
    last_activity_at INTEGER NOT NULL
);

CREATE TABLE tasks (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    seq INTEGER NOT NULL,
    type TEXT NOT NULL,
    reviews_task_id INTEGER REFERENCES tasks(id),
    title TEXT NOT NULL,
    state TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT '',
    dispatch_rule TEXT NOT NULL DEFAULT '',
    landing_mode TEXT NOT NULL,
    autonomy_review TEXT NOT NULL DEFAULT 'ask',
    autonomy_land TEXT NOT NULL DEFAULT 'ask',
    branch TEXT NOT NULL DEFAULT '',
    base_ref TEXT NOT NULL DEFAULT '',
    worktree_path TEXT NOT NULL DEFAULT '',
    herdr_workspace_id TEXT NOT NULL DEFAULT '',
    pane_id TEXT NOT NULL DEFAULT '',
    pane_label TEXT NOT NULL DEFAULT '',
    agent_name TEXT NOT NULL DEFAULT '',
    agent_session TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    landed_ref TEXT NOT NULL DEFAULT '',
    last_output_hash TEXT NOT NULL DEFAULT '',
    last_worktree_hash TEXT NOT NULL DEFAULT '',
    last_progress_at INTEGER NOT NULL DEFAULT 0,
    agent_absent_since INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(project_id, seq)
);

CREATE TABLE transitions (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    source TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL
);

CREATE TABLE signals (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    verb TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    data_json TEXT NOT NULL DEFAULT '{}',
    at INTEGER NOT NULL
);

CREATE TABLE notices (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id),
    task_id INTEGER REFERENCES tasks(id),
    kind TEXT NOT NULL,
    summary TEXT NOT NULL,
    data_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    delivered_at INTEGER,
    acked_at INTEGER
);

CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    delivered_at INTEGER,
    status TEXT NOT NULL DEFAULT 'queued'
);

CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    received_at INTEGER NOT NULL,
    kind TEXT NOT NULL,
    pane_id TEXT NOT NULL DEFAULT '',
    data_json TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE approvals (
    id INTEGER PRIMARY KEY,
    task_id INTEGER NOT NULL REFERENCES tasks(id),
    action TEXT NOT NULL,
    user_quote TEXT NOT NULL,
    at INTEGER NOT NULL
);

CREATE TABLE notice_notifications (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id),
    notified_at INTEGER NOT NULL
);

CREATE INDEX tasks_project_state_idx ON tasks(project_id, state);
CREATE INDEX tasks_pane_idx ON tasks(pane_id);
CREATE INDEX notices_open_idx ON notices(project_id, acked_at, delivered_at);
CREATE INDEX messages_queue_idx ON messages(task_id, status, created_at);
CREATE INDEX events_age_idx ON events(received_at);

-- +goose Down
DROP TABLE notice_notifications;
DROP TABLE approvals;
DROP TABLE events;
DROP TABLE messages;
DROP TABLE notices;
DROP TABLE signals;
DROP TABLE transitions;
DROP TABLE tasks;
DROP TABLE projects;
