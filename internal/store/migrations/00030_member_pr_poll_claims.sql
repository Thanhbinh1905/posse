-- +goose Up
CREATE TABLE member_pr_poll_state (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo TEXT NOT NULL,
    pr_polled_at INTEGER NOT NULL DEFAULT 0,
    claim_until INTEGER NOT NULL DEFAULT 0,
    claim_token TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(project_id, repo)
);

-- +goose Down
DROP TABLE member_pr_poll_state;
